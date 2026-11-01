package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI answers the internal decision endpoint with whatever state is set; it also records forge comments.
type fakeAPI struct {
	mu       sync.Mutex
	state    string
	scope    string
	hash     string // "" = echo the request's hash from the last line
	comments []string
	edits    []string
	voids    []string
}

func approvalSetup(t *testing.T) (*server, *fakeAPI, *bytes.Buffer) {
	t.Helper()
	fa := &fakeAPI{state: "pending"}
	var lines bytes.Buffer
	old := eventOut
	eventOut = &lines
	t.Cleanup(func() { eventOut = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/user":
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/o/r/issues/") && r.Method == "GET":
			_, _ = io.WriteString(w, `{"assignees":[{"login":"alice"}]}`)
			return
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/o/r/pulls/"):
			_, _ = io.WriteString(w, `{"head":{"ref":"agent/alice/x"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/void") {
			b, _ := io.ReadAll(r.Body)
			fa.voids = append(fa.voids, r.URL.Path+" "+string(b))
			return
		}
		if r.Method == "PATCH" {
			b, _ := io.ReadAll(r.Body)
			fa.edits = append(fa.edits, r.URL.Path+" "+string(b))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/approvals/") {
			if r.Header.Get("X-Approval-Secret") == "" {
				w.WriteHeader(403)
				return
			}
			h := fa.hash
			if h == "" {
				h = lastRequest(t, &lines).ContentHash
			}
			_ = json.NewEncoder(w).Encode(decision{State: fa.state, Scope: fa.scope, DecidedBy: "bob", ContentHash: h})
			return
		}
		b, _ := io.ReadAll(r.Body)
		fa.comments = append(fa.comments, r.URL.Path+" "+string(b))
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":`+strconv.Itoa(700+len(fa.comments))+`}`)
	}))
	t.Cleanup(srv.Close)
	s := newServer(&Config{ForgeURL: srv.URL, ForgeUser: "alice", AgentName: "alice", Token: "t", Repos: []string{"o/r"}, ForgeKind: "gitea"})
	s.enableApprovals(srv.URL, time.Minute)
	s.appr.poll = 5 * time.Millisecond
	return s, fa, &lines
}

func lastRequest(t *testing.T, b *bytes.Buffer) approvalRequest {
	var r approvalRequest
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(l, approvalPrefix) {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(l, approvalPrefix)), &r)
		}
	}
	return r
}

func mcpCall(t *testing.T, h http.Handler, job, tool string) map[string]any {
	t.Helper()
	return mcpCallInput(t, h, job, tool, `{"command":"npm i"}`)
}

func mcpCallInput(t *testing.T, h http.Handler, job, tool, input string) map[string]any {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"` + tool + `","input":` + input + `}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Agentdev-Job", job)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("mcp answer: %s", rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(resp.Result.Content[0].Text), &out)
	return out
}

func TestApprovalPromptAllowJob(t *testing.T) {
	s, fa, lines := approvalSetup(t)
	fa.state, fa.scope = "allowed", "job"
	h := s.routes()
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "allow" {
		t.Fatalf("want allow, got %v", out)
	}
	r := lastRequest(t, lines)
	if r.Job != "issue-3" || r.Tool != "Bash" || r.ContentHash != approvalHash(r.ID, "alice", "o/r", "issue-3", "claude", "Bash", r.Input) {
		t.Fatalf("request line: %+v", r)
	}
	if len(fa.comments) != 1 || !strings.Contains(fa.comments[0], "/issues/3/comments") || !strings.Contains(fa.comments[0], "/approve "+r.ID) {
		t.Fatalf("comment: %v", fa.comments)
	}
	// allow_job: the same tool in the same job does not ask again
	fa.state = "denied"
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "allow" {
		t.Fatalf("allow_job not kept: %v", out)
	}
	// allow_job covers the same input only
	if out := mcpCallInput(t, h, "issue-3", "Bash", `{"command":"rm -rf /"}`); out["behavior"] != "deny" {
		t.Fatalf("allow_job covered another input: %v", out)
	}
	if r := lastRequest(t, lines); r.RequesterID != 42 || len(r.SecretHash) != 64 || strings.Contains(lines.String(), `"secret"`) {
		t.Fatalf("requester/secret: %+v", r)
	}
	// another job asks again (nothing granted beyond the job)
	if out := mcpCall(t, h, "issue-4", "Bash"); out["behavior"] != "deny" {
		t.Fatalf("another job: %v", out)
	}
}

func TestApprovalFailsClosed(t *testing.T) {
	s, fa, _ := approvalSetup(t)
	h := s.routes()
	fa.state, fa.hash = "allowed", strings.Repeat("0", 64) // decision for other content
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "deny" {
		t.Fatalf("hash mismatch allowed: %v", out)
	}
	if out := mcpCall(t, h, "", "Bash"); out["behavior"] != "deny" { // no job
		t.Fatalf("no job allowed: %v", out)
	}
	fa.state, fa.hash = "pending", ""
	s.appr.timeout = 20 * time.Millisecond
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "deny" {
		t.Fatalf("timeout allowed: %v", out)
	}
	s.appr.apiURL = "http://127.0.0.1:1" // API unreachable
	if out := mcpCall(t, h, "issue-3", "Edit"); out["behavior"] != "deny" {
		t.Fatalf("unreachable API allowed: %v", out)
	}
}

func TestApprovalPushHeld(t *testing.T) {
	s, fa, lines := approvalSetup(t)
	s.appr.pushHold = 20 * time.Millisecond
	ctx := context.WithValue(context.Background(), ctxJob{}, "pr-9")
	paths := []string{".github/workflows/a.yml"}
	err := s.git.approve(ctx, "agent/alice/x", "abc", paths)
	if err == nil || !strings.Contains(err.Error(), "waits for approval") {
		t.Fatalf("pending push: %v", err)
	}
	first := lastRequest(t, lines).ID
	// timeout: same commit again reuses the open record (no new line, no new comment); the comment says "still open"
	_ = s.git.approve(ctx, "agent/alice/x", "abc", paths)
	if lastRequest(t, lines).ID != first || len(fa.comments) != 1 {
		t.Fatalf("a retry after the wait opened a new request: %d comments", len(fa.comments))
	}
	if len(fa.edits) != 1 || !strings.Contains(fa.edits[0], "still open") {
		t.Fatalf("comment not updated once: %v", fa.edits)
	}
	// a new commit on the same branch and paths supersedes the old request: void, then a new one
	_ = s.git.approve(ctx, "agent/alice/x", "def", paths)
	if lastRequest(t, lines).ID == first || len(fa.voids) != 1 || !strings.Contains(fa.voids[0], first) ||
		!strings.Contains(fa.voids[0], "superseded") {
		t.Fatalf("supersede: voids=%v", fa.voids)
	}
	second := lastRequest(t, lines).ID
	fa.state = "allowed"
	if err := s.git.approve(ctx, "agent/alice/x", "def", paths); err != nil {
		t.Fatalf("allowed push: %v", err)
	}
	if lastRequest(t, lines).ID != second {
		t.Fatal("allowed push opened a new request")
	}
	fa.state = "denied"
	if err := s.git.approve(ctx, "agent/alice/x", "ghi", paths); err == nil || !isPolicy(err) {
		t.Fatalf("denied push: %v", err)
	}
}

func TestApprovalPromptDedupAndJobDone(t *testing.T) {
	s, fa, lines := approvalSetup(t)
	s.appr.timeout = 20 * time.Millisecond
	h := s.routes()
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "deny" {
		t.Fatalf("timeout allowed: %v", out)
	}
	first := lastRequest(t, lines).ID
	// the same prompt again: same record, no new comment
	_ = mcpCall(t, h, "issue-3", "Bash")
	if lastRequest(t, lines).ID != first || len(fa.comments) != 1 {
		t.Fatalf("duplicate request: %d comments", len(fa.comments))
	}
	// a decision on the open record applies the next time it asks
	fa.state = "allowed"
	if out := mcpCall(t, h, "issue-3", "Bash"); out["behavior"] != "allow" {
		t.Fatalf("decision on open record not used: %v", out)
	}
	// another input is another request
	fa.state = "pending"
	_ = mcpCallInput(t, h, "issue-3", "Bash", `{"command":"make"}`)
	if lastRequest(t, lines).ID == first || len(fa.comments) != 2 {
		t.Fatalf("other input did not open a request: %d", len(fa.comments))
	}
	// the job ends: the open record becomes void and its comment says so
	req := httptest.NewRequest("POST", "/job-done?job=issue-3", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || len(fa.voids) != 1 || !strings.Contains(fa.voids[0], "the job ended") {
		t.Fatalf("job-done: %d %v", rec.Code, fa.voids)
	}
	last := fa.edits[len(fa.edits)-1]
	if !strings.Contains(last, "void") || !strings.Contains(last, "has no effect") || strings.Contains(last, "/approve ") {
		t.Fatalf("void comment: %s", last)
	}
	if len(s.appr.reqs) != 0 {
		t.Fatalf("records left: %v", s.appr.reqs)
	}
}

func TestApprovalEndToEndPush(t *testing.T) {
	f := newFixture(t)
	s, fa, _ := approvalSetup(t)
	s.git = f.g
	s.enableApprovals(s.appr.apiURL, time.Minute)
	s.appr.poll = 5 * time.Millisecond
	fa.state = "allowed"
	f.commit(t, "agent/alice/w", ".github/workflows/ci.yml", "x")
	ctx := context.WithValue(context.Background(), ctxJob{}, "issue-1")
	if _, err := f.g.Push(ctx, "agent/alice/w", f.bundle(t)); err != nil {
		t.Fatalf("approved protected push: %v", err)
	}
}

func TestNoDecisionComments(t *testing.T) {
	for _, b := range []string{"/approve 0123456789abcdef", "ok\n  /deny x", "/APPROVE-JOB 1"} {
		raw, _ := json.Marshal(map[string]string{"body": b})
		if err := checkNoDecision(nil, raw); err == nil || !isPolicy(err) {
			t.Errorf("%q: not refused", b)
		}
	}
	raw, _ := json.Marshal(map[string]string{"body": "waiting for approval, reply with `/approve id`"})
	if err := checkNoDecision(nil, raw); err != nil {
		t.Errorf("mid-line mention refused: %v", err)
	}
}

func TestMCPHandshake(t *testing.T) {
	s, _, _ := approvalSetup(t)
	h := s.routes()
	for _, m := range []string{"initialize", "tools/list"} {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+m+`"}`))
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"result"`) {
			t.Fatalf("%s: %d %s", m, rec.Code, rec.Body)
		}
	}
}

func TestApprovalForeignJob(t *testing.T) {
	s, fa, _ := approvalSetup(t)
	fa.state = "allowed"
	s.c.ForgeUser = "mallory" // issue is assigned to alice, PR branch is alice's
	h := s.routes()
	for _, job := range []string{"issue-3", "pr-9"} {
		if out := mcpCall(t, h, job, "Bash"); out["behavior"] != "deny" {
			t.Fatalf("%s: foreign job allowed: %v", job, out)
		}
	}
	if len(fa.comments) != 0 {
		t.Fatalf("commented on a foreign job: %v", fa.comments)
	}
}

func TestRedact(t *testing.T) {
	s, _, _ := approvalSetup(t)
	// the token-shaped value is built at run time so the secret scanner does not flag the test source
	in := "run -H \"Author" + "ization: Bea" + "rer abcdefghijkl\" -d password=hunter2xyz t " + "gh" + "p_" + strings.Repeat("0123456789", 3) + ` tok`
	out := s.appr.redact(in)
	for _, leak := range []string{"abcdefghijkl", "hunter2xyz", "0123456789"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "run -H") {
		t.Fatalf("over-redacted: %s", out)
	}
}
