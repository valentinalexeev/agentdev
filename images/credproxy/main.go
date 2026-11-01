// Command credproxy is the agentdev Claude credential proxy (#66, security model phase 3, fixes R11, closes R47).
//
// Agent containers call it as ANTHROPIC_BASE_URL with a per-agent key (keys.go). It checks the key, enforces the
// agent's token budget (budget.go), replaces the key with the real Claude credential of that agent and forwards the
// request to the Anthropic API. It reads the rate-limit headers and token counts of every response itself (usage.go)
// and prints them as AGENTDEV_USAGE lines for the operator, so usage no longer comes from inside the agent pod.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// credStore reads the credentials from a directory with one file per Claude Secret (claude-shared, claude-<agent>),
// each with KEY=VALUE lines. The file is re-read on each lookup, so a rotated Secret takes effect without a restart.
type credStore struct{ dir string }

// lookup returns claude-<agent> if it holds a credential, else claude-shared (same rule as 06-agents.sh and the
// playbook). The values never leave this process except in the upstream request header.
func (c credStore) lookup(agent string) (credential, bool) {
	for _, name := range []string{"claude-" + agent, "claude-shared"} {
		data, err := os.ReadFile(filepath.Join(c.dir, name))
		if err != nil {
			continue
		}
		cr := credential{secret: name}
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "CLAUDE_CODE_OAUTH_TOKEN":
				cr.oauth = strings.TrimSpace(v)
			case "ANTHROPIC_API_KEY":
				cr.apiKey = strings.TrimSpace(v)
			}
		}
		if cr.oauth != "" || cr.apiKey != "" {
			return cr, true
		}
	}
	return credential{}, false
}

// allowedPath: only the model endpoints Claude Code needs are forwarded.
func allowedPath(p string) bool {
	return p == "/v1/messages" || p == "/v1/messages/count_tokens"
}

type ctxKey struct{}

type reqInfo struct {
	agent string
	cred  credential
}

type server struct {
	signing  []byte
	creds    credStore
	budget   *budget
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	now      func() time.Time
}

func newServer(signing []byte, creds credStore, b *budget, upstream *url.URL, transport http.RoundTripper) *server {
	s := &server{signing: signing, creds: creds, budget: b, upstream: upstream, now: time.Now}
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
	u := usageLine{Agent: info.agent, Secret: info.cred.secret}
	fromHeaders(&u, resp.StatusCode, resp.Header, now)
	if resp.Request.URL.Path != "/v1/messages" { // count_tokens: no spend, but the headers still count
		if u.FiveHour != nil || u.Weekly != nil || u.RateLimited {
			emitLine(usagePrefix, u)
		}
		return nil
	}
	m := &tokenMeter{sse: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")}
	resp.Body = &meteredBody{ReadCloser: resp.Body, m: m, done: func(in, out int64) {
		u.InputTokens, u.OutputTokens = in, out
		s.budget.add(info.agent, in+out)
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
		_ = json.NewEncoder(w).Encode(map[string]any{"budgets": s.budget.snapshot()})
		return
	}
	agent := verifyKey(s.signing, callerKey(r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")))
	if agent == "" {
		emitEvent("", "proxy-key-invalid", "medium", r.Method+" "+clip(r.URL.Path, 100)+": missing or wrong agent key")
		apiError(w, http.StatusUnauthorized, "authentication_error", "claude-proxy: missing or wrong agent key")
		return
	}
	if r.Method != http.MethodPost || !allowedPath(r.URL.Path) {
		emitEvent(agent, "proxy-path", "medium", r.Method+" "+clip(r.URL.Path, 100)+" is not forwarded")
		apiError(w, http.StatusForbidden, "permission_error", "claude-proxy: only POST /v1/messages is forwarded")
		return
	}
	if ok, used, limit := s.budget.allow(agent); !ok {
		if s.budget.firstRefusal(agent) {
			emitEvent(agent, "budget-exhausted", "low", "token budget of the agent used: "+strconv.FormatInt(used, 10)+" of "+strconv.FormatInt(limit, 10)+" in 24 h")
		}
		w.Header().Set("Retry-After", "3600")
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "claude-proxy: the token budget of agent "+agent+" for 24 h is used")
		return
	}
	cred, ok := s.creds.lookup(agent)
	if !ok {
		apiError(w, http.StatusServiceUnavailable, "api_error", "claude-proxy: no Claude credential for agent "+agent)
		return
	}
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqInfo{agent, cred})))
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
	up, err := url.Parse(envOr("UPSTREAM", "https://api.anthropic.com"))
	if err != nil || up.Scheme == "" {
		log.Fatal("bad UPSTREAM")
	}
	def, err := strconv.ParseInt(envOr("CLAUDE_BUDGET_TOKENS", "0"), 10, 64)
	if err != nil || def < 0 {
		log.Fatal("CLAUDE_BUDGET_TOKENS must be a non-negative integer")
	}
	b := newBudget(def, os.Getenv("BUDGET_DIR"), os.Getenv("STATE_FILE"))
	s := newServer(signing, credStore{envOr("CREDENTIALS_DIR", "/etc/claude-proxy/credentials")}, b, up, http.DefaultTransport)
	srv := &http.Server{Addr: envOr("LISTEN", ":8080"), Handler: s, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	log.Printf("claude-proxy listening on %s, upstream %s, default budget %d", srv.Addr, up.Host, def)
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
