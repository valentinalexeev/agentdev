package main

import (
	"net/http"
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
