package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ghFake is a fake GitHub REST API: the App flow (installation lookup, access token), collaborator permissions in
// GitHub's answer shape (permission + role_name), issue comments and state. down: every call answers 502.
type ghFake struct {
	*httptest.Server
	mu       sync.Mutex
	down     bool
	perms    map[string][2]string // login -> permission, role_name
	comments []map[string]any
	state    string
	calls    []string
	selfDown bool // GET /user (the static token's own user) answers 502
	selfID   int64
}

const (
	ghInstToken   = "ghs_installationtoken0123456789"
	ghStaticToken = "ghp_statictoken0123456789abcdef"
)

func newGHFake(t *testing.T) *ghFake {
	f := &ghFake{state: "open", perms: map[string][2]string{
		"wendy": {"write", "write"}, "mona": {"write", "maintain"}, "adam": {"admin", "admin"},
		"rita": {"read", "read"}, "tria": {"read", "triage"}, "nora": {"none", ""},
	}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if f.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		auth := r.Header.Get("Authorization")
		app := strings.HasPrefix(auth, "Bearer ey") // the App JWT
		switch {
		case r.URL.Path == "/repos/owner/x/installation" && app:
			_, _ = w.Write([]byte(`{"id":4242}`))
		case r.URL.Path == "/app/installations/4242/access_tokens" && app && r.Method == "POST":
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Permissions {
				if v != "read" {
					t.Errorf("installation token asks %s=%s: the API must stay read-only", k, v)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": ghInstToken,
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case r.URL.Path == "/app" && app:
			_, _ = w.Write([]byte(`{"slug":"my-agentdev"}`))
		case r.URL.Path == "/user" && auth == "Bearer "+ghStaticToken:
			if f.selfDown {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": f.selfID})
		case auth != "Bearer "+ghInstToken && auth != "Bearer "+ghStaticToken:
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/x/collaborators/") && strings.HasSuffix(r.URL.Path, "/permission"):
			u := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/owner/x/collaborators/"), "/permission")
			p, ok := f.perms[u]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": p[0], "role_name": p[1], "user": map[string]string{"login": u}})
		case r.URL.Path == "/repos/owner/x/issues/3/comments":
			_ = json.NewEncoder(w).Encode(f.comments)
		case r.URL.Path == "/repos/owner/x/issues/3":
			_ = json.NewEncoder(w).Encode(map[string]string{"state": f.state})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *ghFake) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }

// forge builds the API's GitHub client as main does: App id and a key file from the environment.
func (f *ghFake) forge(t *testing.T) *githubForge {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_APP_ID": "123", "GITHUB_APP_PRIVATE_KEY_FILE": p, "GITHUB_API_URL": "https://unused.invalid"}
	g, err := githubFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	g.api, g.hc = f.URL, f.Client() // the fake is plain http
	return g
}

func TestGitHubPermissionMapping(t *testing.T) {
	f := newGHFake(t)
	g := f.forge(t)
	for user, want := range map[string]perm{"adam": permAdmin, "wendy": permWrite, "mona": permWrite, "rita": permRead,
		"tria": permRead, "nora": permNone, "stranger": permNone} {
		got, err := g.Permission(context.Background(), "github", "owner/x", user)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", user, got, err, want)
		}
	}
	// one installation token for all of them (cached)
	n := 0
	for _, c := range f.calls {
		if strings.HasSuffix(c, "/access_tokens") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("minted %d installation tokens, want 1", n)
	}
	f.setDown(true)
	if _, err := g.Permission(context.Background(), "github", "owner/x", "wendy"); err == nil {
		t.Fatal("error from GitHub must be an error (fail closed), not a permission")
	}
}

func TestGitHubFromEnv(t *testing.T) {
	if g, err := githubFromEnv(func(string) string { return "" }); g != nil || err != nil {
		t.Fatalf("nothing configured: %v %v", g, err)
	}
	if _, err := githubFromEnv(func(k string) string { return map[string]string{"GITHUB_APP_ID": "x1"}[k] }); err == nil {
		t.Fatal("bad app id accepted")
	}
	if _, err := githubFromEnv(func(k string) string {
		return map[string]string{"GITHUB_API_TOKEN": "github_pat_0123456789abcdefghij", "GITHUB_URL": "http://ghe.local"}[k]
	}); err == nil {
		t.Fatal("plain http GitHub URL accepted")
	}
	g, err := githubFromEnv(func(k string) string {
		return map[string]string{"GITHUB_API_TOKEN": "github_pat_0123456789abcdefghij", "GITHUB_URL": "https://ghe.example.com"}[k]
	})
	if err != nil || g.api != "https://ghe.example.com/api/v3" || g.static == "" {
		t.Fatalf("GHES token: %+v %v", g, err)
	}
}

// newGHEnv: agent alice on GitHub owner/x; the API checks permissions on the fake GitHub (no Gitea at all).
func newGHEnv(t *testing.T) (*tenv, *ghFake) {
	t.Helper()
	f := newGHFake(t)
	e := newEnv(t, "")
	u, _ := e.k.Get(context.Background(), "alice")
	_ = unstructured.SetNestedField(u.Object, "github", "spec", "forge")
	if _, err := e.k.d.Resource(agentsGVR).Namespace("coding-agents").Update(context.Background(), u, updateOpts); err != nil {
		t.Fatal(err)
	}
	wr := &writer{store: e.k, perms: forgePerms{gitea: newGiteaPerms("", ""), github: f.forge(t)}, adminGroup: "agentdev-admins", usage: e.k}
	e.h = routes(idVerify, e.k, nil, wr)
	return e, f
}

func TestGitHubAgentChanges(t *testing.T) {
	testIDs["nora"] = &identity{Subject: "s-o", Username: "nora", ForgeLogin: "nora"}
	defer delete(testIDs, "nora")
	e, f := newGHEnv(t)
	cases := []struct {
		name, method, path, token, body string
		code                            int
		grant                           string
	}{
		{"write pauses", "PATCH", "/api/agents/alice", "writer", `{"paused":true}`, http.StatusOK, "repo-write"},
		{"write resumes", "PATCH", "/api/agents/alice", "writer", `{"paused":false,"maxConcurrent":3}`, http.StatusOK, "repo-write"},
		{"read refused", "PATCH", "/api/agents/alice", "reader", `{"paused":true}`, http.StatusForbidden, ""},
		{"none refused", "PATCH", "/api/agents/alice", "nora", `{"paused":true}`, http.StatusForbidden, ""},
		{"write cannot add", "POST", "/api/agents", "writer", `{"name":"bob","repo":"owner/x","forge":"github"}`, http.StatusForbidden, ""},
		{"admin adds", "POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x","forge":"github"}`, http.StatusCreated, "repo-admin"},
		{"write cannot remove", "DELETE", "/api/agents/bob", "writer", ``, http.StatusForbidden, ""},
		{"admin removes", "DELETE", "/api/agents/bob", "admin", ``, http.StatusNoContent, "repo-admin"},
	}
	for _, c := range cases {
		w := e.call(c.method, c.path, c.token, c.body)
		if w.Code != c.code {
			t.Fatalf("%s: code %d %s", c.name, w.Code, w.Body)
		}
		if a := e.lastAudit(t); c.grant != "" && a.Grant != c.grant {
			t.Fatalf("%s: grant %q, want %q", c.name, a.Grant, c.grant)
		}
	}
	// GitHub fails: 503 for repository users, the admin group still works (no lockout)
	f.setDown(true)
	if w := e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GitHub down: code %d %s", w.Code, w.Body)
	}
	if w := e.call("PATCH", "/api/agents/alice", "admin", `{"paused":true}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GitHub down, repo admin: code %d", w.Code)
	}
	if w := e.call("PATCH", "/api/agents/alice", "group", `{"paused":true}`); w.Code != http.StatusOK {
		t.Fatalf("GitHub down, agentdev-admins pause: code %d %s", w.Code, w.Body)
	}
	if w := e.call("POST", "/api/agents", "group", `{"name":"carl","repo":"owner/x","forge":"github"}`); w.Code != http.StatusCreated {
		t.Fatalf("GitHub down, agentdev-admins add: code %d %s", w.Code, w.Body)
	}
}

func TestGitHubWithoutCredentialFailsClosed(t *testing.T) {
	e := newEnv(t, "")
	u, _ := e.k.Get(context.Background(), "alice")
	_ = unstructured.SetNestedField(u.Object, "github", "spec", "forge")
	_, _ = e.k.d.Resource(agentsGVR).Namespace("coding-agents").Update(context.Background(), u, updateOpts)
	wr := &writer{store: e.k, perms: forgePerms{gitea: newGiteaPerms("", "")}, adminGroup: "agentdev-admins", usage: e.k}
	e.h = routes(idVerify, e.k, nil, wr)
	if w := e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no GitHub credential: code %d", w.Code)
	}
	if w := e.call("PATCH", "/api/agents/alice", "group", `{"paused":true}`); w.Code != http.StatusOK {
		t.Fatalf("agentdev-admins: code %d", w.Code)
	}
}

func ghApprovalObj(t0 time.Time) *unstructured.Unstructured {
	u := approvalObj("carol", aid, "owner/x", t0)
	_ = unstructured.SetNestedField(u.Object, "github", "spec", "forge")
	_ = unstructured.SetNestedField(u.Object, int64(201234567), "spec", "requesterId") // the App bot's id
	return u
}

func ghComment(id int64, login, typ, body string, at time.Time) map[string]any {
	return map[string]any{"id": id, "user": map[string]any{"id": id + 1000, "login": login, "type": typ}, "body": body,
		"created_at": at.Format(time.RFC3339), "updated_at": at.Format(time.RFC3339)}
}

func TestGitHubApprovalDecisions(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := t0.Add(time.Minute)
	cases := []struct {
		name     string
		comments []map[string]any
		state    string
		by       string
		refused  bool
	}{
		{"write approves", []map[string]any{ghComment(1, "wendy", "User", "/approve "+aid, at)}, "allowed", "wendy", false},
		{"maintain denies", []map[string]any{ghComment(1, "mona", "User", "/deny "+aid, at)}, "denied", "mona", false},
		{"admin approves", []map[string]any{ghComment(1, "adam", "User", "/approve-job "+aid, at)}, "allowed", "adam", false},
		{"read refused", []map[string]any{ghComment(1, "rita", "User", "/approve "+aid, at)}, "pending", "", true},
		{"triage refused", []map[string]any{ghComment(1, "tria", "User", "/approve "+aid, at)}, "pending", "", true},
		{"none refused", []map[string]any{ghComment(1, "nora", "User", "/approve "+aid, at)}, "pending", "", true},
		// the agents all write as the App bot: refused by its login (deny list) and by type Bot
		{"agent (App bot) refused", []map[string]any{ghComment(1, "my-agentdev[bot]", "Bot", "/approve "+aid, at)}, "pending", "", true},
		{"App bot login as User refused", []map[string]any{ghComment(1, "My-Agentdev[bot]", "User", "/approve "+aid, at)}, "pending", "", true},
		{"other bot refused", []map[string]any{ghComment(1, "wendy", "Bot", "/approve "+aid, at)}, "pending", "", true},
		{"first valid wins", []map[string]any{ghComment(1, "rita", "User", "/approve "+aid, at),
			ghComment(2, "wendy", "User", "/deny "+aid, at.Add(time.Second))}, "denied", "wendy", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGHFake(t)
			f.comments = c.comments
			g := f.forge(t)
			k, buf := approvalEnv(t, ghApprovalObj(t0))
			d := newDecider(k, agentsFixed{{Name: "carol", Forge: "github"}}, &fakeComments{}, forgePerms{github: g})
			d.github = g
			d.now = func() time.Time { return t0.Add(5 * time.Minute) }
			if err := d.pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			a := status(t, k, "carol-"+aid)
			if a.State != c.state || a.DecidedBy != c.by {
				t.Fatalf("state %s by %q, want %s by %q", a.State, a.DecidedBy, c.state, c.by)
			}
			if got := strings.Contains(buf.String(), `"result":"denied"`); got != c.refused {
				t.Fatalf("refused audit %v, want %v: %s", got, c.refused, buf)
			}
		})
	}
}

func TestGitHubApprovalFailClosed(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	f := newGHFake(t)
	f.comments = []map[string]any{ghComment(1, "wendy", "User", "/approve "+aid, t0.Add(time.Minute))}
	g := f.forge(t)
	k, _ := approvalEnv(t, ghApprovalObj(t0))
	d := newDecider(k, agentsFixed{{Name: "carol", Forge: "github"}}, &fakeComments{}, forgePerms{github: g})
	d.github = g
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	f.setDown(true)
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("GitHub down: state %s, want pending", a.State)
	}
	// no GitHub client at all: a GitHub request stays pending, nobody decides
	d.github = nil
	f.setDown(false)
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("no GitHub client: state %s, want pending", a.State)
	}
	// back up: the decision applies
	d.github = g
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "wendy" {
		t.Fatalf("GitHub up: %+v", a)
	}
}

// With a static GITHUB_API_TOKEN its own user never decides; while GitHub does not say who that is, nobody decides.
func TestGitHubStaticTokenSelfFailsClosed(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	f := newGHFake(t)
	f.comments = []map[string]any{ghComment(1, "wendy", "User", "/approve "+aid, t0.Add(time.Minute))}
	f.selfDown = true
	env := map[string]string{"GITHUB_API_TOKEN": ghStaticToken, "GITHUB_API_URL": "https://unused.invalid"}
	g, err := githubFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	g.api, g.hc = f.URL, f.Client()
	k, _ := approvalEnv(t, ghApprovalObj(t0))
	d := newDecider(k, agentsFixed{{Name: "carol", Forge: "github"}}, &fakeComments{}, forgePerms{github: g})
	d.github = g
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("own user unknown: state %s, want pending", a.State)
	}
	// GitHub answers: wendy is the token's own user (id 1001), so her comment is refused
	f.mu.Lock()
	f.selfDown, f.selfID = false, 1001
	f.mu.Unlock()
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("token's own user decided: state %s", a.State)
	}
	// another user id: wendy decides
	f.mu.Lock()
	f.selfID = 7
	f.mu.Unlock()
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "wendy" {
		t.Fatalf("own user known: %+v", a)
	}
}

func TestGitHubApprovalClosedIssueVoids(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	f := newGHFake(t)
	f.state = "closed"
	g := f.forge(t)
	k, _ := approvalEnv(t, ghApprovalObj(t0))
	d := newDecider(k, agentsFixed{{Name: "carol", Forge: "github"}}, &fakeComments{}, forgePerms{github: g})
	d.github = g
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "void" {
		t.Fatalf("closed issue on GitHub: state %s, want void", a.State)
	}
}

// The web UI decide path (#67, #138) uses the same GitHub check: write decides, read does not see it, GitHub down is
// 503 and the admin group still decides.
func TestGitHubUIDecision(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	f := newGHFake(t)
	ag := agentObj("carol", "owner/x", false)
	_ = unstructured.SetNestedField(ag.Object, "github", "spec", "forge")
	k, _ := approvalEnv(t, ag, ghApprovalObj(t0))
	old := auditDB
	auditDB = &memAudit{}
	t.Cleanup(func() { auditDB = old })
	wr := &writer{store: k, perms: forgePerms{gitea: newGiteaPerms("", ""), github: f.forge(t)}, adminGroup: "agentdev-admins"}
	h := routes(idVerify, k, nil, wr)
	if w := postDecision(h, "reader", "", body("approve")); w.Code != http.StatusNotFound {
		t.Fatalf("GitHub read user: %d %s", w.Code, w.Body)
	}
	f.setDown(true)
	if w := postDecision(h, "writer", "", body("approve")); w.Code == http.StatusOK {
		t.Fatalf("GitHub down: decided (%d)", w.Code)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("GitHub down: %+v", a)
	}
	f.setDown(false)
	if w := postDecision(h, "writer", "", body("approve")); w.Code != http.StatusOK {
		t.Fatalf("GitHub write user: %d %s", w.Code, w.Body)
	}
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "wendy" {
		t.Fatalf("UI decision on GitHub: %+v", a)
	}
}
