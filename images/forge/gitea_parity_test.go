package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #237 P1, P3, P6: the Gitea rules for commit statuses, the labeler check and review requests
func TestGiteaParityRules(t *testing.T) {
	forge, calls := fakeForge(t)
	s := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	h := s.routes()
	sha := "0123456789abcdef0123456789abcdef01234567"
	ok := []struct{ m, p, b string }{
		{"GET", "/api/v1/repos/owner/repo/commits/" + sha + "/status", ""},
		{"GET", "/api/v1/repos/owner/repo/commits/" + sha + "/statuses?limit=50", ""},
		{"GET", "/api/v1/repos/owner/repo/issues/3/timeline?limit=50&page=1", ""},
		{"GET", "/api/v1/repos/owner/repo/assignees", ""},
		{"POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["owner"]}`},
		{"POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["dev.user_1","b"]}`},
	}
	for _, c := range ok {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != 200 {
			t.Errorf("%s %s: %d %s", c.m, c.p, rec.Code, rec.Body)
		}
	}
	n := len(*calls)
	refused := []struct{ name, m, p, b string }{
		{"short sha", "GET", "/api/v1/repos/owner/repo/commits/abc/status", ""},
		{"branch name for status", "GET", "/api/v1/repos/owner/repo/commits/main/status", ""},
		{"post a status", "POST", "/api/v1/repos/owner/repo/statuses/" + sha, `{"state":"success"}`},
		{"other repo status", "GET", "/api/v1/repos/other/repo/commits/" + sha + "/status", ""},
		{"collaborator list", "GET", "/api/v1/repos/owner/repo/collaborators", ""},
		{"add a collaborator", "PUT", "/api/v1/repos/owner/repo/collaborators/mallory", `{"permission":"admin"}`},
		{"another user's permission", "GET", "/api/v1/repos/owner/repo/collaborators/owner/permission", ""},
		{"team reviewers", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"team_reviewers":["core"]}`},
		{"team with users", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["owner"],"team_reviewers":["core"]}`},
		{"no reviewers", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":[]}`},
		{"six reviewers", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["a","b","c","d","e","f"]}`},
		{"not a login", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["a/b"]}`},
		{"not JSON", "POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `reviewers=owner`},
	}
	for _, c := range refused {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", c.name, rec.Code)
		}
	}
	if len(*calls) != n {
		t.Fatalf("refused calls reached the forge: %v", (*calls)[n:])
	}
}

// #241 P5: on Gitea the agent may add exactly its own label agent:<name>, only to a PR from its own branch
func TestGiteaOwnPRLabel(t *testing.T) {
	var writes []string
	pulls := map[string]string{
		"7": `{"head":{"ref":"agent/alice/issue-3","repo":{"full_name":"owner/repo"}}}`,
		"8": `{"head":{"ref":"agent/bob/issue-4","repo":{"full_name":"owner/repo"}}}`,
		"9": `{"head":{"ref":"agent/alice/issue-3","repo":{"full_name":"mallory/repo"}}}`, // a fork
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			writes = append(writes, r.Method+" "+r.URL.Path)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		n := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/owner/repo/pulls/")
		if b, ok := pulls[n]; ok {
			_, _ = io.WriteString(w, b)
			return
		}
		http.NotFound(w, r) // issue 3 is not a PR
	}))
	t.Cleanup(srv.Close)
	s := newServer(&Config{ForgeURL: srv.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	h := s.routes()
	if rec := call(t, h, "POST", "/api/v1/repos/owner/repo/issues/7/labels", `{"labels":["agent:alice"]}`); rec.Code/100 != 2 {
		t.Fatalf("own label on own PR: %d %s", rec.Code, rec.Body)
	}
	if len(writes) != 1 || writes[0] != "POST /api/v1/repos/owner/repo/issues/7/labels" {
		t.Fatalf("label call not forwarded: %v", writes)
	}
	refused := []struct{ name, p, b string }{
		{"another agent's label", "/api/v1/repos/owner/repo/issues/7/labels", `{"labels":["agent:bob"]}`},
		{"two labels", "/api/v1/repos/owner/repo/issues/7/labels", `{"labels":["agent:alice","bug"]}`},
		{"label id", "/api/v1/repos/owner/repo/issues/7/labels", `{"labels":[5]}`},
		{"bob's PR", "/api/v1/repos/owner/repo/issues/8/labels", `{"labels":["agent:alice"]}`},
		{"a fork", "/api/v1/repos/owner/repo/issues/9/labels", `{"labels":["agent:alice"]}`},
		{"an issue (self-assign work)", "/api/v1/repos/owner/repo/issues/3/labels", `{"labels":["agent:alice"]}`},
		{"other repo", "/api/v1/repos/other/repo/issues/7/labels", `{"labels":["agent:alice"]}`},
	}
	for _, c := range refused {
		if rec := call(t, h, "POST", c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", c.name, rec.Code)
		}
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if rec := call(t, h, m, "/api/v1/repos/owner/repo/issues/7/labels", `{"labels":["agent:alice"]}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s labels: want 403, got %d", m, rec.Code)
		}
	}
	if len(writes) != 1 {
		t.Fatalf("refused label calls reached Gitea: %v", writes[1:])
	}
}
