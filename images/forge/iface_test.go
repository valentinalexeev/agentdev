package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitea: issues, PRs, labels and comments for the #236 rules; records every call.
func fakeGitea(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var calls []string
	pr := func(n, ref, repo, title string) string {
		return `{"number":` + n + `,"title":"` + title + `","head":{"ref":"` + ref + `","repo":{"full_name":"` + repo + `"}}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/owner/repo")
		if r.Method != "GET" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		switch p {
		case "/issues/3": // assigned to alice
			w.Write([]byte(`{"number":3,"assignees":[{"login":"alice"}]}`))
		case "/issues/4": // assigned to bob
			w.Write([]byte(`{"number":4,"assignees":[{"login":"bob"}]}`))
		case "/issues/7", "/issues/8", "/issues/9":
			w.Write([]byte(`{"pull_request":{"merged":false}}`))
		case "/pulls/7":
			w.Write([]byte(pr("7", "agent/alice/issue-3", "owner/repo", "WIP: [WIP] fix it")))
		case "/pulls/8":
			w.Write([]byte(pr("8", "agent/bob/issue-4", "owner/repo", "bob")))
		case "/pulls/9":
			w.Write([]byte(pr("9", "agent/alice/issue-3", "mallory/repo", "fork")))
		case "/pulls/7/files":
			w.Write([]byte(`[{"filename":"a.go"}]`))
		case "/labels/1":
			w.Write([]byte(`{"id":1,"name":"needs-decision"}`))
		case "/labels/2":
			w.Write([]byte(`{"id":2,"name":"approved"}`))
		case "/issues/comments/11":
			w.Write([]byte(`{"id":11,"user":{"login":"alice"},"body":"x"}`))
		case "/issues/comments/12":
			w.Write([]byte(`{"id":12,"user":{"login":"bob"},"body":"x"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func noWrite(t *testing.T, name string, cs []string) {
	t.Helper()
	for _, c := range cs {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("%s: a write reached the forge: %s", name, c)
		}
	}
}

func TestGiteaForgeInterface236(t *testing.T) {
	forge, calls := fakeGitea(t)
	s := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	h := s.routes()
	base := "/api/v1/repos/owner/repo"
	ok := []struct{ name, m, p, b string }{
		{"needs-decision on an assigned issue", "POST", base + "/issues/3/labels", `{"labels":["needs-decision"]}`},
		{"in-progress on own PR", "POST", base + "/issues/7/labels", `{"labels":["in-progress"]}`},
		{"agent label on own PR", "POST", base + "/issues/7/labels", `{"labels":["agent:alice"]}`},
		{"remove needs-decision (by id)", "DELETE", base + "/issues/3/labels/1", ""},
		{"draft PR (WIP title)", "POST", base + "/pulls", `{"title":"WIP: x","head":"agent/alice/issue-3","base":"main","body":"b"}`},
		{"edit own comment", "PATCH", base + "/issues/comments/11", `{"body":"updated"}`},
		{"PR files", "GET", base + "/pulls/7/files", ""},
	}
	for _, c := range ok {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != 200 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	n := len(calls())
	if rec := call(t, h, "POST", base+"/pulls/7/ready_for_review", ""); rec.Code != 200 {
		t.Fatalf("ready own PR: %d %s", rec.Code, rec.Body)
	}
	last := calls()[len(calls())-1]
	if last != `PATCH /api/v1/repos/owner/repo/pulls/7 {"title":"fix it"}` || len(calls()) != n+3 {
		t.Fatalf("ready: %v", calls()[n:])
	}
	refused := []struct{ name, m, p, b string }{
		{"label not on the allow-list", "POST", base + "/issues/3/labels", `{"labels":["approved"]}`},
		{"allowed with a forbidden one", "POST", base + "/issues/3/labels", `{"labels":["in-progress","approved"]}`},
		{"agent label on an issue (self-assign)", "POST", base + "/issues/3/labels", `{"labels":["agent:alice"]}`},
		{"another agent's label", "POST", base + "/issues/7/labels", `{"labels":["agent:bob"]}`},
		{"issue of another agent", "POST", base + "/issues/4/labels", `{"labels":["needs-decision"]}`},
		{"PR of another agent", "POST", base + "/issues/8/labels", `{"labels":["in-progress"]}`},
		{"fork PR with an agent-like branch", "POST", base + "/issues/9/labels", `{"labels":["in-progress"]}`},
		{"unknown issue", "POST", base + "/issues/99/labels", `{"labels":["in-progress"]}`},
		{"label ids instead of names", "POST", base + "/issues/3/labels", `{"labels":[1]}`},
		{"extra key", "POST", base + "/issues/3/labels", `{"labels":["in-progress"],"x":1}`},
		{"replace labels", "PUT", base + "/issues/3/labels", `{"labels":["in-progress"]}`},
		{"clear all labels", "DELETE", base + "/issues/3/labels", ""},
		{"remove a forbidden label", "DELETE", base + "/issues/3/labels/2", ""},
		{"remove an unknown label", "DELETE", base + "/issues/3/labels/77", ""},
		{"remove by name on Gitea", "DELETE", base + "/issues/3/labels/needs-decision", ""},
		{"remove on another agent's issue", "DELETE", base + "/issues/4/labels/1", ""},
		{"edit another user's comment", "PATCH", base + "/issues/comments/12", `{"body":"x"}`},
		{"edit an unknown comment", "PATCH", base + "/issues/comments/13", `{"body":"x"}`},
		{"edit with extra keys", "PATCH", base + "/issues/comments/11", `{"body":"x","user":"bob"}`},
		{"edit to a decision", "PATCH", base + "/issues/comments/11", `{"body":"/approve 5"}`},
		{"ready on another agent's PR", "POST", base + "/pulls/8/ready_for_review", ""},
		{"ready on a fork PR", "POST", base + "/pulls/9/ready_for_review", ""},
		{"ready with a body", "POST", base + "/pulls/7/ready_for_review", `{"title":"merge me"}`},
		{"edit a PR directly", "PATCH", base + "/pulls/7", `{"title":"x"}`},
		{"PR files of another repo", "GET", "/api/v1/repos/other/repo/pulls/7/files", ""},
	}
	for _, c := range refused {
		n := len(calls())
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d %s", c.name, rec.Code, rec.Body)
		}
		noWrite(t, c.name, calls()[n:])
	}
}

func TestGitHubForgeInterface236(t *testing.T) {
	d, _, calls := newTestGitHub(t)
	s := newServer(d.c)
	s.drv, s.rules = d, d.rules()
	h := s.routes()
	base := "/api/v1/repos/octo-org/repo"
	ok := []struct{ name, m, p, b string }{
		{"needs-decision on an issue labelled for alice", "POST", base + "/issues/12/labels", `{"labels":["needs-decision"]}`},
		{"agent label on own PR", "POST", base + "/issues/14/labels", `{"labels":["agent:alice"]}`},
		{"remove in-progress (by name)", "DELETE", base + "/issues/12/labels/in-progress", ""},
		{"draft PR", "POST", base + "/pulls", `{"title":"t","head":"agent/alice/issue-12","base":"main","body":"b","draft":true}`},
		{"edit own signed comment", "PATCH", base + "/issues/comments/501", `{"body":"updated"}`},
		{"PR files", "GET", base + "/pulls/14/files?per_page=100", ""},
		{"ready own PR", "POST", base + "/pulls/14/ready_for_review", ""},
	}
	for _, c := range ok {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code/100 != 2 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	var sawEdit, sawGraphQL bool
	for _, c := range calls() {
		if c.method == "PATCH" && strings.Contains(c.body, "agent `alice`") {
			sawEdit = true
		}
		if c.method == "POST" && c.path == "/graphql" && strings.Contains(c.body, "PR_kwDOtest14") && strings.Contains(c.body, "markPullRequestReadyForReview") {
			sawGraphQL = true
		}
	}
	if !sawEdit || !sawGraphQL {
		t.Fatalf("edit signed %v, graphql %v: %+v", sawEdit, sawGraphQL, calls())
	}
	refused := []struct{ name, p, m, b string }{
		{"agent label on an issue", base + "/issues/12/labels", "POST", `{"labels":["agent:alice"]}`},
		{"issue given to bob", base + "/issues/13/labels", "POST", `{"labels":["needs-decision"]}`},
		{"bob's PR", base + "/issues/15/labels", "POST", `{"labels":["in-progress"]}`},
		{"other label", base + "/issues/12/labels", "POST", `{"labels":["approved"]}`},
		{"remove own routing label", base + "/issues/14/labels/agent%3Aalice", "DELETE", ""},
		{"remove other label", base + "/issues/12/labels/approved", "DELETE", ""},
		{"edit bob's comment (same App bot)", base + "/issues/comments/502", "PATCH", `{"body":"x"}`},
		{"edit a person's comment", base + "/issues/comments/503", "PATCH", `{"body":"x"}`},
		{"ready on bob's PR", base + "/pulls/15/ready_for_review", "POST", ""},
		{"ready on a fork", base + "/pulls/16/ready_for_review", "POST", ""},
		{"draft from a fork", base + "/pulls", "POST", `{"title":"t","head":"mallory:agent/alice/x","base":"main","body":"b","draft":true}`},
	}
	for _, c := range refused {
		var before []ghCall = calls()
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d %s", c.name, rec.Code, rec.Body)
		}
		for _, x := range calls()[len(before):] {
			if x.method != "GET" && !strings.Contains(x.path, "access_tokens") {
				t.Errorf("%s: a write reached GitHub: %+v", c.name, x)
			}
		}
	}
}

func TestETagCache(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	body := `[{"number":1}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("If-None-Match"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/repos/owner/repo/issues/5" { // no ETag: never cached
			w.Write([]byte(`{"n":5}`))
			return
		}
		if r.Header.Get("If-None-Match") == `W/"v1"` && body == `[{"number":1}]` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if body == `[{"number":1}]` {
			w.Header().Set("ETag", `W/"v1"`)
		} else {
			w.Header().Set("ETag", `W/"v2"`)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	s := newServer(&Config{ForgeURL: srv.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	h := s.routes()
	p := "/api/v1/repos/owner/repo/issues?state=open"
	r1 := call(t, h, "GET", p, "")
	r2 := call(t, h, "GET", p, "")
	if r1.Code != 200 || r2.Code != 200 || r2.Body.String() != `[{"number":1}]` || r2.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("304 not served from the cache: %d %d %q", r1.Code, r2.Code, r2.Body)
	}
	body = `[{"number":2}]`
	if r3 := call(t, h, "GET", p, ""); r3.Body.String() != `[{"number":2}]` {
		t.Fatalf("changed resource: %q", r3.Body)
	}
	call(t, h, "GET", "/api/v1/repos/owner/repo/issues/5", "")
	call(t, h, "GET", "/api/v1/repos/owner/repo/issues/5", "")
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, "|") != `|W/"v1"|W/"v1"||` {
		t.Fatalf("If-None-Match sent: %q", seen)
	}
	// bounded
	var c etagCache
	for i := 0; i < etagMaxEntries+10; i++ {
		c.put(strings.Repeat("u", i+1), &etagEntry{etag: "x", body: []byte("b")})
	}
	if len(c.m) != etagMaxEntries || len(c.order) != etagMaxEntries || c.get("u") != nil {
		t.Fatalf("cache not bounded: %d %d", len(c.m), len(c.order))
	}
	c.put("big", &etagEntry{etag: "x", body: make([]byte, etagMaxBody+1)})
	if c.get("big") != nil {
		t.Fatal("oversized body cached")
	}
}
