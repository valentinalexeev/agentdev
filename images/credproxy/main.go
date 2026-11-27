// Command credproxy is the agentdev Claude credential proxy (#66, security model phase 3, fixes R11, closes R47).
//
// Agent containers call it as ANTHROPIC_BASE_URL with a per-agent key (keys.go). It checks the key, enforces the
// agent's budget in cost units (prices.go) (budget.go), replaces the key with the real Claude credential of that agent and forwards the
// request to the Anthropic API. It reads the rate-limit headers and token counts of every response itself (usage.go)
// and prints them as AGENTDEV_USAGE lines for the operator, so usage no longer comes from inside the agent pod.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type credential struct {
	secret string // name of the Claude Secret it came from (the subscription, for #93's pause logic)
	oauth  string // CLAUDE_CODE_OAUTH_TOKEN
	apiKey string // ANTHROPIC_API_KEY
}

// credStore reads the credentials from a directory with one subdirectory per Claude Secret (claude-shared,
// claude-<agent>), each mounted as a Secret volume (one file per key). Files are re-read on each lookup, so a rotated
// Secret takes effect without a restart.
type credStore struct{ dir string }

func readKey(dir, key string) string {
	b, err := os.ReadFile(filepath.Join(dir, key))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// lookup returns claude-<agent> if it holds a credential, else claude-shared (same rule as the playbooks kind.yml
// and deploy.yml). The values never leave this process except in the upstream request header.
func (c credStore) lookup(agent string) (credential, bool) {
	for _, name := range []string{"claude-" + agent, "claude-shared"} {
		d := filepath.Join(c.dir, name)
		cr := credential{secret: name, oauth: readKey(d, "CLAUDE_CODE_OAUTH_TOKEN"), apiKey: readKey(d, "ANTHROPIC_API_KEY")}
		if cr.oauth != "" || cr.apiKey != "" {
			return cr, true
		}
	}
	return credential{}, false
}

// allowedPath: only the model endpoints Claude Code needs are forwarded. It compares the escaped path, so an encoded
// form such as /v1%2Fmessages is refused rather than forwarded as written (review #119).
func allowedPath(u *url.URL) bool {
	p := u.EscapedPath()
	// Claude Code adds ?beta=true; no other query is forwarded
	return (p == "/v1/messages" || p == "/v1/messages/count_tokens") && (u.RawQuery == "" || u.RawQuery == "beta=true")
}

type ctxKey struct{}

type reqInfo struct {
	agent string
	job   string // X-Agentdev-Job, checked (jobs.go); "" = none
	cred  credential
	start time.Time
	call  *classifierCall // auto mode classifier request (#506); nil for others
}

type server struct {
	signing   []byte
	creds     credStore
	budget    *budget
	prices    *prices
	jobs      *jobs
	tel       *telemetry     // nil = no metrics (tests)
	jobLimit  int64          // CLAUDE_JOB_BUDGET_UNITS (#196 D6): loop guard per job id; 0 = off
	approval  approvalConfig // CLAUDE_JOB_APPROVAL_UNITS (#310): soft limit per job, raised only by an approval
	approvals approvalSource // allowed cost Approvals (approval.go); nil = none can raise the soft limit
	upstream  *url.URL
	proxy     *httputil.ReverseProxy
	now       func() time.Time
	authMode  string         // CLAUDE_PROXY_AUTH: key, token or both (#352)
	reviewer  *tokenReviewer // nil when authMode is key
}

func newServer(signing []byte, creds credStore, b *budget, upstream *url.URL, transport http.RoundTripper) *server {
	s := &server{signing: signing, creds: creds, budget: b, prices: newPrices(""), jobs: newJobs(), upstream: upstream, now: time.Now, authMode: authKey}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.observe,
		FlushInterval:  -1, // stream server-sent events at once
		Transport:      transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error: %v", err)
			apiError(w, http.StatusBadGateway, "api_error", "claude-proxy: upstream not reachable")
		},
	}
	return s
}

// rewrite replaces every caller credential with the agent's real one.
func (s *server) rewrite(pr *httputil.ProxyRequest) {
	info := pr.In.Context().Value(ctxKey{}).(reqInfo)
	pr.SetURL(s.upstream)
	pr.Out.Host = s.upstream.Host
	h := pr.Out.Header
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Cookie")
	h.Del("Proxy-Authorization")
	h.Del(jobHeader) // attribution only (#196 D3): never sent upstream
	// review #119: a client-chosen encoding (gzip) would reach the meter compressed and count 0 tokens. Without the
	// header the transport asks for gzip itself and decompresses before the meter sees the body.
	h.Del("Accept-Encoding")
	if info.cred.apiKey != "" {
		h.Set("X-Api-Key", info.cred.apiKey)
	} else {
		h.Set("Authorization", "Bearer "+info.cred.oauth)
		if !strings.Contains(h.Get("Anthropic-Beta"), "oauth-2025-04-20") {
			h.Set("Anthropic-Beta", strings.Trim(h.Get("Anthropic-Beta")+",oauth-2025-04-20", ","))
		}
	}
}

// observe reads the rate-limit headers and meters the body; the usage line is printed when the body ends.
func (s *server) observe(resp *http.Response) error {
	info := resp.Request.Context().Value(ctxKey{}).(reqInfo)
	now := s.now()
	u := usageLine{Agent: info.agent, Secret: info.cred.secret, Job: info.job}
	fromHeaders(&u, resp.StatusCode, resp.Header, now)
	s.tel.window(info.cred.secret, "5h", u.FiveHour)
	s.tel.window(info.cred.secret, "7d", u.Weekly)
	if u.RateLimited {
		s.tel.refuse(info.agent, "rate_limited")
	}
	if resp.Request.URL.Path != "/v1/messages" { // count_tokens: no spend, but the headers still count
		if u.FiveHour != nil || u.Weekly != nil || u.RateLimited {
			emitLine(usagePrefix, u)
		}
		return nil
	}
	sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if info.call != nil && resp.StatusCode == http.StatusOK {
		resp.Body = &classifierBody{ReadCloser: resp.Body, r: replyText{sse: sse}, agent: info.agent, call: info.call}
	}
	m := &tokenMeter{sse: sse}
	resp.Body = &meteredBody{ReadCloser: resp.Body, m: m, done: func(t tokenCounts, model string) {
		p := s.prices.get()
		units := p.units(model, t)
		usd, hasUSD := p.usdOf(units)
		s.tel.record(resp.Request.Context(), info.agent, info.job, p.modelLabel(model), t, units, usd, hasUSD, info.start, s.now(), resp.StatusCode)
		u.Model, u.Units, u.Tokens, u.Multiplier = model, units, t.byName(), p.multiplier(model)
		u.InputTokens, u.OutputTokens = t.input(), t[tokOut]
		u.CacheRead, u.CacheWrite = t[tokCacheRead], t[tokCacheWrite5m]+t[tokCacheWrite1h]
		s.budget.add(info.agent, units)
		s.jobs.add(info.agent, info.job, units, t)
		emitLine(usagePrefix, u)
	}}
	return nil
}

// apiError answers in the Anthropic error shape, so Claude Code reports it clearly.
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusNoContent)
		return
	case "/status": // budgets for the API and the UI; no secrets, no keys
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		p := s.prices.get()
		bs := s.budget.snapshot()
		for i := range bs {
			if usd, ok := p.usdOf(bs[i].Used); ok {
				bs[i].UsedUSD = &usd
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"unit": "units", "weights": p.weightsByName(), "budgets": bs, "jobs": s.jobs.snapshot(p),
			"approvalStepUnits": s.approval.step}) // #310: units one cost approval adds (0 = soft limit off)
		return
	}
	agent, kind := s.authenticate(r)
	if agent == "" {
		s.tel.refuse("", "bad_key")
		emitEvent("", "proxy-key-invalid", "medium", r.Method+" "+clip(r.URL.Path, 100)+": missing or wrong agent "+kind+" (auth mode "+s.authMode+")")
		apiError(w, http.StatusUnauthorized, "authentication_error", "claude-proxy: missing or wrong agent key")
		return
	}
	if r.Method != http.MethodPost || !allowedPath(r.URL) {
		emitEvent(agent, "proxy-path", "medium", r.Method+" "+clip(r.URL.Path, 100)+" is not forwarded")
		apiError(w, http.StatusForbidden, "permission_error", "claude-proxy: only POST /v1/messages is forwarded")
		return
	}
	if ok, used, limit := s.budget.allow(agent); !ok {
		s.tel.refuse(agent, "budget")
		if s.budget.firstRefusal(agent) {
			emitEvent(agent, "budget-exhausted", "low", "budget of the agent used: "+strconv.FormatInt(used, 10)+" of "+strconv.FormatInt(limit, 10)+" units in 24 h")
		}
		w.Header().Set("Retry-After", "3600")
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "claude-proxy: the budget of agent "+agent+" for 24 h is used")
		return
	}
	job := cleanJob(r.Header.Get(jobHeader))
	// loop guard (#196 D6): the job id comes from the agent, so this only stops a run that loops, it is not a
	// security bound (the per-agent budget above is)
	if s.jobLimit > 0 && job != "" {
		if used := s.jobs.used(agent, job); used >= s.jobLimit {
			s.tel.refuse(agent, "job_budget")
			if s.jobs.firstRefusal(agent, job) {
				emitEvent(agent, "job-budget-exhausted", "low", "job "+job+" used "+strconv.FormatInt(used, 10)+" of "+strconv.FormatInt(s.jobLimit, 10)+" units (CLAUDE_JOB_BUDGET_UNITS)")
			}
			apiError(w, http.StatusTooManyRequests, "rate_limit_error", "claude-proxy: job "+job+" of agent "+agent+" used its budget (CLAUDE_JOB_BUDGET_UNITS)")
			return
		}
	}
	if refused := s.softLimit(w, agent, job); refused {
		return
	}
	cred, ok := s.creds.lookup(agent)
	if !ok {
		apiError(w, http.StatusServiceUnavailable, "api_error", "claude-proxy: no Claude credential for agent "+agent)
		return
	}
	call := s.readClassifier(r)
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqInfo{agent, job, cred, s.now(), call})))
}

// readClassifier reads the request body to find an auto mode classifier request (#506) and puts the same bytes back,
// so the request goes upstream unchanged. A body over classifierBodyCap is passed through unread.
func (s *server) readClassifier(r *http.Request) *classifierCall {
	if r.URL.Path != "/v1/messages" || r.Body == nil {
		return nil
	}
	if r.ContentLength > classifierBodyCap {
		classifierLogOnce("large", "request over the 4 MiB inspection cap")
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, classifierBodyCap+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil {
		return nil
	}
	if len(head) > classifierBodyCap {
		classifierLogOnce("large", "request over the 4 MiB inspection cap")
		return nil
	}
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			classifierLogOnce("encoded", "gzip request body not readable")
			return nil
		}
		plain, err := io.ReadAll(io.LimitReader(zr, classifierBodyCap+1))
		if err != nil || len(plain) > classifierBodyCap {
			classifierLogOnce("large", "request over the 4 MiB inspection cap")
			return nil
		}
		head = plain // only for detection; the compressed bytes go upstream
	default:
		classifierLogOnce("encoded", "request with Content-Encoding "+clip(enc, 40))
		return nil
	}
	return detectClassifier(head)
}

// authenticate returns the agent and the kind of credential seen ("token", "key" or "credential" when none). A JWT
// goes to TokenReview (modes token, both), anything else to the HMAC check (modes key, both). The identity comes only
// from the credential, never from another header.
func (s *server) authenticate(r *http.Request) (string, string) {
	c := callerKey(r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	switch {
	case c == "":
		return "", "credential"
	case looksLikeJWT(c):
		if s.authMode == authKey || s.reviewer == nil {
			return "", "token"
		}
		return s.reviewer.verify(r.Context(), c), "token"
	default:
		if s.authMode == authToken {
			return "", "key"
		}
		return verifyKey(s.signing, c), "key"
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func main() {
	signing, err := os.ReadFile(envOr("SIGNING_KEY_FILE", "/etc/claude-proxy/signing/key"))
	if err != nil || len(strings.TrimSpace(string(signing))) < 32 {
		log.Fatal("signing key missing or shorter than 32 bytes")
	}
	signing = []byte(strings.TrimSpace(string(signing)))
	if len(os.Args) == 3 && os.Args[1] == "key" { // credproxy key <agent>: print the agent's key (used by the deploy)
		if !agentRe.MatchString(os.Args[2]) {
			log.Fatal("bad agent name")
		}
		os.Stdout.WriteString(agentKey(signing, os.Args[2]))
		return
	}
	if os.Getenv("MODE") == "broker" { // credential broker for project secrets (#202, broker.go)
		eventSource = "credential-broker"
		bk := newBroker(signing, envOr("ROUTES_FILE", "/etc/credential-broker/routes/routes.json"),
			envOr("VALUES_DIR", "/etc/credential-broker/values"), os.Getenv("SELF_URL"), brokerTransport())
		srv := &http.Server{Addr: envOr("LISTEN", ":8080"), Handler: bk, ReadHeaderTimeout: 10 * time.Second}
		log.Printf("credential-broker listening on %s", srv.Addr)
		serve(srv, nil)
		return
	}
	up, err := url.Parse(envOr("UPSTREAM", "https://api.anthropic.com"))
	// https only (review #119); plain http only for a local test with ALLOW_HTTP_UPSTREAM=true
	if err != nil || up.Host == "" || (up.Scheme != "https" && !(up.Scheme == "http" && os.Getenv("ALLOW_HTTP_UPSTREAM") == "true")) {
		log.Fatal("UPSTREAM must be an https URL")
	}
	def, warn, err := budgetFromEnv(os.Getenv) // #196: CLAUDE_BUDGET_TOKENS deprecated, removed in 2.0.0
	if warn != "" {
		log.Print("WARNING: " + warn)
	}
	if err != nil {
		log.Fatal(err)
	}
	b := newBudget(def, os.Getenv("BUDGET_DIR"), os.Getenv("STATE_FILE"))
	jobLimit, err := strconv.ParseInt(envOr("CLAUDE_JOB_BUDGET_UNITS", "0"), 10, 64)
	if err != nil || jobLimit < 0 {
		log.Fatal("CLAUDE_JOB_BUDGET_UNITS must be a non-negative integer")
	}
	creds := credStore{envOr("CREDENTIALS_DIR", "/etc/claude-proxy/credentials")}
	// #381: refuse a bad credential at start (the playbook no longer reads the secrets to check them)
	if err := creds.checkCredentials(func(secret string) {
		log.Printf("WARNING: Claude secret %s has no recognised credential (CLAUDE_CODE_OAUTH_TOKEN= or ANTHROPIC_API_KEY=)", secret)
		emitEvent(strings.TrimPrefix(secret, "claude-"), "claude-credential-format", "high", "secret "+secret+" ignored at start")
	}); err != nil {
		log.Fatal(err)
	}
	s := newServer(signing, creds, b, up, http.DefaultTransport)
	s.prices = newPrices(os.Getenv("PRICES_DIR"))
	s.jobLimit = jobLimit
	if s.approval, err = approvalFromEnv(os.Getenv, jobLimit); err != nil {
		log.Fatal(err)
	}
	if s.approval.soft > 0 {
		ns := envOr("AGENTS_NAMESPACE", os.Getenv("POD_NAMESPACE"))
		ka, kerr := inClusterApprovals(ns)
		if kerr != nil || !agentRe.MatchString(ns) {
			// fail closed: the soft limit holds, no approval can raise it
			log.Printf("WARNING: cannot read Approvals (%v): CLAUDE_JOB_APPROVAL_UNITS is a hard stop", kerr)
		} else {
			s.approvals = ka
		}
	}
	tel, err := newTelemetry(os.Getenv("TELEMETRY_OTLP_URL"), b)
	if err != nil {
		log.Fatal(err)
	}
	s.tel = tel
	s.authMode = envOr("CLAUDE_PROXY_AUTH", authBoth)
	switch s.authMode {
	case authKey:
	case authToken, authBoth:
		ns := envOr("AGENTS_NAMESPACE", os.Getenv("POD_NAMESPACE"))
		if !agentRe.MatchString(ns) {
			log.Fatal("AGENTS_NAMESPACE (or POD_NAMESPACE) must name the namespace of the agent service accounts")
		}
		if s.reviewer, err = inClusterReviewer(ns); err != nil {
			if s.authMode == authToken {
				log.Fatalf("CLAUDE_PROXY_AUTH=token needs the in-cluster API: %v", err)
			}
			log.Printf("token auth off, HMAC keys only: %v", err)
		}
	default:
		log.Fatal("CLAUDE_PROXY_AUTH must be key, token or both")
	}
	srv := &http.Server{Addr: envOr("LISTEN", ":8080"), Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("claude-proxy listening on %s, upstream %s, default budget %d units, auth %s", srv.Addr, up.Host, def, s.authMode)
	serve(srv, func(ctx context.Context) {
		if addr := envOr("METRICS_LISTEN", ":9464"); addr != "off" { // #196 D4: separate port, scraper only (NetworkPolicy)
			go tel.serveMetrics(ctx, addr)
		}
	})
}

// serve runs srv until SIGTERM, then shuts it down within 30 s. start runs once with the server's context.
func serve(srv *http.Server, start func(context.Context)) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if start != nil {
		start(ctx)
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
