package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type server struct {
	c      *Config
	git    *gitRepo
	client *http.Client
	rules  []rule
	appr   *approvals // nil: approvals off (approvals.go, #67)
	drv    driver     // forge-specific API location, auth and allow-list (driver.go)

	usageGate usageGate // POST /usage rate limit (usage.go)
}

type ctxJob struct{}

// rule is one allowed Gitea REST call. The path pattern's first group is the repo (owner/name), checked against REPO.
type rule struct {
	method string
	path   *regexp.Regexp
	check  func(s *server, body []byte) error // optional body policy
	// optional body rewrite after the check (GitHub: the agent's signature line on what it writes)
	rewrite func(s *server, body []byte) ([]byte, error)
}

const repoRe = `/repos/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`

func newServer(c *Config) *server {
	s := &server{c: c, git: &gitRepo{c: c},
		client: &http.Client{Timeout: 60 * time.Second,
			// never follow a redirect: it could carry the Authorization header to another host
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	s.drv = driverOrGitea(c)
	s.rules = s.drv.rules()
	return s
}

// event reports a refusal (see events.go); job is the loop's X-Agentdev-Job header, if any
func (s *server) event(r *http.Request, rule, severity, action string) {
	job := ""
	if r != nil {
		job = r.Header.Get("X-Agentdev-Job")
	}
	emitEvent(s.c.AgentName, job, rule, severity, action)
}

func (s *server) logf(format string, a ...any) {
	log.Printf("%s [%s] forge: %s", time.Now().UTC().Format("15:04:05"), s.c.ForgeUser, fmt.Sprintf(format, a...))
}

// checkNoDecision refuses a write whose JSON carries an approval decision command (/approve, /approve-job, /deny) in
// any string value, at any depth and under any key: agents never decide (docs/security-model.md, decision rule 1).
// The API ignores such comments from agents anyway; refusing them here makes the attempt a security event.
// Keys that differ only in case are refused too: Go folds them into one struct field while a forge may read another
// (review of #116: {"body":"/approve x","Body":"y"} passed a struct decode but reached GitHub as "body").
func checkNoDecision(_ *server, body []byte) error {
	refused := refuseAs("approval-self-decision", "high", "comment refused: agents cannot post approval decisions")
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		if decisionCmdRe.Match(body) { // not JSON: look at the raw text
			return refused
		}
		return nil
	}
	// exactly one JSON value: trailing data could be read by a forge parser that this check never saw (review of #116)
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return refuseAs("approval-self-decision", "high", "refused: trailing data after the JSON body")
	}
	if hasDecision(v) {
		return refused
	}
	if foldedDupKeys(body) {
		return refuseAs("approval-self-decision", "high", "refused: JSON keys that differ only in case")
	}
	return nil
}

func hasDecision(v any) bool {
	switch x := v.(type) {
	case string:
		return decisionCmdRe.MatchString(x)
	case map[string]any:
		for _, e := range x {
			if hasDecision(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasDecision(e) {
				return true
			}
		}
	}
	return false
}

// foldedDupKeys reports an object that has two keys equal under case folding, at any depth.
func foldedDupKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		obj    bool
		keys   map[string]bool
		expKey bool
	}
	var st []*frame
	for {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		top := func() *frame {
			if len(st) == 0 {
				return nil
			}
			return st[len(st)-1]
		}
		switch tk := t.(type) {
		case json.Delim:
			switch tk {
			case '{':
				if f := top(); f != nil && f.obj {
					f.expKey = true
				}
				st = append(st, &frame{obj: true, keys: map[string]bool{}, expKey: true})
			case '[':
				if f := top(); f != nil && f.obj {
					f.expKey = true
				}
				st = append(st, &frame{})
			case '}', ']':
				st = st[:len(st)-1]
				if f := top(); f != nil && f.obj {
					f.expKey = true
				}
			}
		default:
			f := top()
			if f != nil && f.obj && f.expKey {
				k := strings.ToLower(fmt.Sprint(tk))
				if f.keys[k] {
					return true
				}
				f.keys[k] = true
				f.expKey = false
			} else if f != nil && f.obj {
				f.expKey = true
			}
		}
	}
}

// enableApprovals turns protected-path refusals and Claude prompts into approval requests.
func (s *server) enableApprovals(apiURL string, timeout time.Duration) {
	s.appr = newApprovals(s, apiURL, timeout)
	s.git.approve = func(ctx context.Context, branch, tip string, paths []string) error {
		job, _ := ctx.Value(ctxJob{}).(string)
		return s.appr.pushApproval(ctx, job, branch, tip, paths)
	}
}

// checkOwnHead: a PR may only be opened from the agent's own branch.
func checkOwnHead(s *server, body []byte) error {
	var pr struct {
		Head string `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return refuse("bad PR body")
	}
	return checkBranch(s.c.ForgeUser, pr.Head)
}

func (s *server) repoAllowed(repo string) bool {
	for _, r := range s.c.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/api/v1/", s.proxy)
	mux.HandleFunc("POST /sync", s.sync)
	mux.HandleFunc("POST /push", s.push)
	mux.HandleFunc("POST /idle", s.idle)
	if s.appr != nil {
		mux.HandleFunc("/mcp", s.appr.mcp)               // Claude's permission prompt tool (approvals.go)
		mux.HandleFunc("POST /job-done", s.appr.jobDone) // the loop: a job ended, its open requests become void
	}
	mux.HandleFunc("POST /usage", s.usage)
	mux.HandleFunc("GET /identity", s.identity)
	return s.localOnly(mux)
}

// localOnly refuses anything not from loopback, even if FORGE_API_LISTEN were misconfigured to a pod address.
func (s *server) localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.RemoteAddr, "127.") && !strings.HasPrefix(r.RemoteAddr, "[::1]") {
			s.event(r, "api-non-loopback", "high", fmt.Sprintf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr))
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// policyEvent reports err if it is a policy refusal (not a forge or git failure)
func (s *server) policyEvent(r *http.Request, err error, action string) {
	var pe *policyError
	if errors.As(err, &pe) {
		s.event(r, pe.rule, pe.severity, action+": "+pe.msg)
	}
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var pe *policyError
	if errors.As(err, &pe) {
		code = http.StatusForbidden
	}
	http.Error(w, err.Error(), code)
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var match *rule
	otherRepo := false
	for i := range s.rules {
		ru := &s.rules[i]
		if ru.method != r.Method {
			continue
		}
		if m := ru.path.FindStringSubmatch(p); m != nil {
			if len(m) < 2 || s.repoAllowed(m[1]) {
				match = ru
				break
			}
			otherRepo = true
		}
	}
	if match == nil {
		s.logf("refused %s %s", r.Method, p)
		if otherRepo {
			s.event(r, "api-other-repo", "high", r.Method+" "+p)
		} else {
			s.event(r, "api-not-allowed", "medium", r.Method+" "+p)
		}
		http.Error(w, fmt.Sprintf("forge sidecar: %s %s is not an allowed call", r.Method, p), http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if match.check != nil {
		if err := match.check(s, body); err != nil {
			s.logf("refused %s %s: %v", r.Method, p, err)
			s.policyEvent(r, err, r.Method+" "+p)
			fail(w, err)
			return
		}
	}
	// #67: agents never post an approval decision, on any forge driver and on any write that carries text
	// (issue comments, PR bodies, review replies). Driver rules may add checks but cannot skip this one.
	if r.Method != http.MethodGet {
		if err := checkNoDecision(s, body); err != nil {
			s.logf("refused %s %s: %v", r.Method, p, err)
			s.policyEvent(r, err, r.Method+" "+p)
			fail(w, err)
			return
		}
	}
	if match.rewrite != nil {
		if body, err = match.rewrite(s, body); err != nil {
			s.logf("refused %s %s: %v", r.Method, p, err)
			s.policyEvent(r, err, r.Method+" "+p)
			fail(w, err)
			return
		}
		if r.Method != http.MethodGet { // check what is actually sent, after the driver's rewrite
			if err := checkNoDecision(s, body); err != nil {
				s.logf("refused %s %s: %v", r.Method, p, err)
				s.policyEvent(r, err, r.Method+" "+p)
				fail(w, err)
				return
			}
		}
	}
	drv := s.driver()
	if qc, ok := drv.(interface{ checkQuery(url.Values) error }); ok {
		if err := qc.checkQuery(r.URL.Query()); err != nil {
			s.logf("refused %s %s: %v", r.Method, p, err)
			s.policyEvent(r, err, r.Method+" "+p)
			fail(w, err)
			return
		}
	}
	auth, err := drv.authorization(r.Context())
	if err != nil {
		s.logf("forge credential: %v", err)
		fail(w, errors.New("forge credential unavailable"))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, drv.apiURL(p, r.URL.RawQuery), bytes.NewReader(body))
	if err != nil {
		fail(w, err)
		return
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		fail(w, errors.New("forge unreachable"))
		return
	}
	defer resp.Body.Close()
	if o, ok := drv.(interface{ observe(int) }); ok {
		o.observe(resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// rate-limit hints reach the loop, so it can wait instead of retrying at once
	for _, h := range []string{"Retry-After", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// POST /sync: fetch the forge, answer with a bundle of all branches.
func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// buffered so a failure is still an HTTP error; the bundle is the whole repo, so a very large repo needs more than
	// the sidecar's 512Mi limit (operator render.go)
	var buf bytes.Buffer
	if err := s.git.Sync(ctx, &buf); err != nil {
		s.logf("sync failed: %v", err)
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(buf.Bytes())
}

// POST /push?branch=agent/<user>/x, body = git bundle with HEAD = the new tip. A base= parameter (older loops) is
// ignored: the sidecar looks up the default branch itself.
func (s *server) push(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	tip, err := s.git.Push(context.WithValue(r.Context(), ctxJob{}, r.Header.Get("X-Agentdev-Job")), branch, r.Body)
	if err != nil {
		s.logf("push to %s: %v", branch, err)
		s.policyEvent(r, err, "push to "+branch)
		fail(w, err)
		return
	}
	s.logf("pushed %s to %s", tip[:12], branch)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"branch": branch, "sha": tip})
}

// GET /identity: the forge kind and the commit author the loop uses (#2). On GitHub the author is the agent's name
// with the App bot's noreply address, so GitHub links the commit to the App; on Gitea it is the agent's own user.
func (s *server) identity(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"forge": s.driver().kind(), "agent": s.c.AgentName,
		"name": s.c.ForgeUser, "email": s.c.ForgeUser + "@agents.local"}
	if d, ok := s.driver().(*githubDriver); ok {
		login, id, err := d.identity(r.Context())
		if err != nil {
			s.logf("identity: %v", err)
			fail(w, errors.New("forge identity unavailable"))
			return
		}
		out["name"] = s.c.AgentName + " (agentdev)"
		out["email"] = fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
		out["login"] = login
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
