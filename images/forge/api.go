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
}

type ctxJob struct{}

// rule is one allowed Gitea REST call. The path pattern's first group is the repo (owner/name), checked against REPO.
type rule struct {
	method string
	path   *regexp.Regexp
	check  func(s *server, body []byte) error // optional body policy
}

const repoRe = `/repos/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`

func newServer(c *Config) *server {
	s := &server{c: c, git: &gitRepo{c: c},
		client: &http.Client{Timeout: 60 * time.Second,
			// never follow a redirect: it could carry the Authorization header to another host
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r := func(m, p string, chk func(*server, []byte) error) rule {
		return rule{m, regexp.MustCompile("^" + p + "$"), chk}
	}
	// exactly the calls agent-loop.sh makes (issues, PRs, comments, reviews, review requests)
	s.rules = []rule{
		r("GET", `/user`, nil),
		r("GET", repoRe, nil),
		r("GET", repoRe+`/issues`, nil),
		r("GET", repoRe+`/issues/[0-9]+`, nil),
		r("GET", repoRe+`/issues/[0-9]+/comments`, nil),
		r("POST", repoRe+`/issues/[0-9]+/comments`, checkNoDecision),
		r("GET", repoRe+`/pulls`, nil),
		r("POST", repoRe+`/pulls`, checkOwnHead),
		r("GET", repoRe+`/pulls/[0-9]+`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews/[0-9]+/comments`, nil),
		r("POST", repoRe+`/pulls/[0-9]+/requested_reviewers`, nil),
	}
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

// checkNoDecision refuses a comment that contains an approval decision command (/approve, /approve-job, /deny): agents
// never decide (docs/security-model.md, decision rule 1). The API ignores such comments from agents anyway; refusing
// them here makes the attempt a security event.
func checkNoDecision(_ *server, body []byte) error {
	var c struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(body, &c)
	if decisionCmdRe.MatchString(c.Body) {
		return refuseAs("approval-self-decision", "high", "comment refused: agents cannot post approval decisions")
	}
	return nil
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
		mux.HandleFunc("/mcp", s.appr.mcp) // Claude's permission prompt tool (approvals.go)
	}
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
	u := s.c.ForgeURL + "/api/v1" + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
	if err != nil {
		fail(w, err)
		return
	}
	req.Header.Set("Authorization", "token "+s.c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		fail(w, errors.New("forge unreachable"))
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
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
