package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// On GitHub (#87) the sidecar's approval calls go through the driver: the GitHub API with the installation token,
// the requester is the App bot's account id, and the agent's issue is the one labelled agent:<name>.
func TestApprovalsOnGitHub(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var auths []string
	labels := `[{"name":"agent:alice"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/octo-org/repo/installation":
			w.Write(ghFixture(t, "installation.json"))
		case r.URL.Path == "/app/installations/51234567/access_tokens":
			w.WriteHeader(201)
			w.Write(ghFixture(t, "access_token.json"))
		case r.URL.Path == "/app":
			w.Write(ghFixture(t, "app.json"))
		case r.URL.Path == "/users/my-agentdev[bot]":
			auths = append(auths, r.Header.Get("Authorization"))
			w.Write(ghFixture(t, "bot_user.json"))
		case r.URL.Path == "/repos/octo-org/repo/issues/12":
			auths = append(auths, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"number":12,"assignees":[],"labels":`+labels+`}`)
		case r.URL.Path == "/repos/octo-org/repo/issues/12/comments" && r.Method == "POST":
			auths = append(auths, r.Header.Get("Authorization"))
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":9001}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := ghConfig(t, srv.URL)
	d, err := newGH(c)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return ghNow.Add(-30 * time.Minute) }
	var lines strings.Builder
	old := eventOut
	eventOut = &lines
	t.Cleanup(func() { eventOut = old })
	s := newServer(c)
	s.drv, s.rules = d, d.rules()
	s.enableApprovals("http://127.0.0.1:1", time.Minute)

	r, err := s.appr.open("issue-12", "claude", "Bash", `{"command":"npm i"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.RequesterID != 201234567 || r.Forge != "github" || r.commentID != 9001 {
		t.Fatalf("request %+v", r)
	}
	var line approvalRequest
	_ = json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(lines.String()), approvalPrefix)), &line)
	if line.RequesterID != 201234567 {
		t.Fatalf("request line %q", lines.String())
	}
	mu.Lock()
	for _, a := range auths {
		if !strings.HasPrefix(a, "Bearer ghs_") {
			t.Errorf("approval call without the installation token: %q", a)
		}
	}
	for _, p := range paths {
		if strings.Contains(p, "/api/v1/") {
			t.Errorf("Gitea path on GitHub: %s", p)
		}
	}
	labels = `[{"name":"agent:bob"}]` // another agent's issue
	mu.Unlock()
	if _, err := s.appr.open("issue-12", "claude", "Bash", `{}`); err == nil || !strings.Contains(err.Error(), "agent:alice") {
		t.Fatalf("issue of another agent: %v", err)
	}
}
