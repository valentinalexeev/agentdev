package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// Tokens of the tests: each maps to one identity.
var testIDs = map[string]*identity{
	"writer": {Subject: "s-w", Username: "wendy"},
	"reader": {Subject: "s-r", Username: "rita"},
	"admin":  {Subject: "s-a", Username: "adam"},                                      // admin on owner/x
	"group":  {Subject: "s-g", Username: "gina", Groups: []string{"agentdev-admins"}}, // no forge access at all
	"nobody": {Subject: "s-n", Username: ""},
}

func idVerify(_ context.Context, raw string) (*identity, error) {
	if id, ok := testIDs[raw]; ok {
		return id, nil
	}
	return nil, errors.New("bad token")
}

// fakeGitea answers the collaborator permission endpoint like Gitea, with a token check.
func fakeGitea(t *testing.T) *httptest.Server {
	perms := map[string]string{"wendy": "write", "rita": "read", "adam": "admin"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token svc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		const pre = "/api/v1/repos/owner/x/collaborators/"
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, pre) || !strings.HasSuffix(r.URL.Path, "/permission") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, pre), "/permission")
		p, ok := perms[user]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"permission": p})
	}))
}

func agentObj(name, repo string, paused bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentdev.dev/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": "coding-agents"},
		"spec": map[string]any{"repo": repo, "forge": "gitea", "claudeSecret": "claude-shared",
			"maxConcurrent": int64(2), "paused": paused},
	}}
}

type tenv struct {
	h     http.Handler
	k     *kubeAgents
	audit *bytes.Buffer
}

// newEnv: agent alice on owner/x; forgeURL "" means the forge is unreachable.
func newEnv(t *testing.T, forgeURL string) *tenv {
	t.Helper()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList"}, agentObj("alice", "owner/x", false))
	k := &kubeAgents{c: fake.NewSimpleClientset(), d: d, ns: "coding-agents"}
	buf := &bytes.Buffer{}
	old := auditOut
	auditOut = buf
	t.Cleanup(func() { auditOut = old })
	now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	wr := &writer{store: k, perms: newGiteaPerms(forgeURL, "svc"), adminGroup: "agentdev-admins"}
	return &tenv{h: routes(idVerify, k, wr), k: k, audit: buf}
}

func (e *tenv) call(method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/merge-patch+json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *tenv) spec(t *testing.T, name string) map[string]any {
	t.Helper()
	u, err := e.k.Get(context.Background(), name)
	if err != nil {
		return nil
	}
	s, _, _ := unstructured.NestedMap(u.Object, "spec")
	return s
}

func (e *tenv) lastAudit(t *testing.T) auditEntry {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(e.audit.String()), "\n")
	l := lines[len(lines)-1]
	if !strings.HasPrefix(l, "AGENTDEV_AUDIT {") {
		t.Fatalf("no audit line: %q", e.audit.String())
	}
	var a auditEntry
	if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "AGENTDEV_AUDIT ")), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWriteAuthorization(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	createBody := `{"name":"bob","repo":"owner/x"}`
	cases := []struct {
		name, method, path, token, body string
		down                            bool // forge unreachable
		want                            int
		grant                           string
	}{
		{"writer pauses", "PATCH", "/api/agents/alice", "writer", `{"paused":true}`, false, 200, "repo-write"},
		{"writer edits", "PATCH", "/api/agents/alice", "writer", `{"maxConcurrent":4,"scaleToZero":true}`, false, 200, "repo-write"},
		{"reader cannot pause", "PATCH", "/api/agents/alice", "reader", `{"paused":true}`, false, 403, ""},
		{"admin edits", "PATCH", "/api/agents/alice", "admin", `{"paused":false}`, false, 200, "repo-write"},
		{"group pauses without forge", "PATCH", "/api/agents/alice", "group", `{"paused":true}`, true, 200, "admin-group"},
		{"group cannot resume without forge access", "PATCH", "/api/agents/alice", "group", `{"paused":false}`, false, 403, ""},
		{"group cannot edit", "PATCH", "/api/agents/alice", "group", `{"paused":true,"maxConcurrent":3}`, false, 403, ""},
		{"no forge user", "PATCH", "/api/agents/alice", "nobody", `{"paused":true}`, false, 403, ""},
		{"forge down: writer refused", "PATCH", "/api/agents/alice", "writer", `{"paused":true}`, true, 503, ""},
		{"writer cannot create", "POST", "/api/agents", "writer", createBody, false, 403, ""},
		{"admin creates", "POST", "/api/agents", "admin", createBody, false, 201, "repo-admin"},
		{"group creates", "POST", "/api/agents", "group", createBody, true, 201, "admin-group"},
		{"forge down: admin cannot create", "POST", "/api/agents", "admin", createBody, true, 503, ""},
		{"writer cannot delete", "DELETE", "/api/agents/alice", "writer", "", false, 403, ""},
		{"admin deletes", "DELETE", "/api/agents/alice", "admin", "", false, 204, "repo-admin"},
		{"group deletes", "DELETE", "/api/agents/alice?keepState=true", "group", "", true, 204, "admin-group"},
		{"forge down: admin cannot delete", "DELETE", "/api/agents/alice", "admin", "", true, 503, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := gitea.URL
			if c.down {
				url = "http://127.0.0.1:1" // nothing listens
			}
			e := newEnv(t, url)
			before := e.spec(t, "alice")
			w := e.call(c.method, c.path, c.token, c.body)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
			a := e.lastAudit(t)
			if a.Who.Subject != testIDs[c.token].Subject || a.Grant != c.grant {
				t.Errorf("audit who/grant: %+v", a)
			}
			if c.want >= 300 {
				if a.Result == "ok" || a.After != nil {
					t.Errorf("refused change logged as ok: %+v", a)
				}
				if got := e.spec(t, "alice"); !equalJSON(got, before) {
					t.Errorf("agent changed on a refused request: %v -> %v", before, got)
				}
				if c.method == "POST" && e.spec(t, "bob") != nil {
					t.Error("agent created on a refused request")
				}
			} else if a.Result != "ok" {
				t.Errorf("audit result: %+v", a)
			}
		})
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestWriteValidation(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	cases := []struct{ name, method, path, body string }{
		{"empty patch", "PATCH", "/api/agents/alice", `{}`},
		{"not an object", "PATCH", "/api/agents/alice", `[1]`},
		{"bad json", "PATCH", "/api/agents/alice", `{`},
		{"two objects", "PATCH", "/api/agents/alice", `{"paused":true}{"paused":false}`},
		{"repo not editable", "PATCH", "/api/agents/alice", `{"repo":"evil/x"}`},
		{"spec wrapper not allowed", "PATCH", "/api/agents/alice", `{"spec":{"paused":true}}`},
		{"null paused", "PATCH", "/api/agents/alice", `{"paused":null}`},
		{"string paused", "PATCH", "/api/agents/alice", `{"paused":"yes"}`},
		{"maxConcurrent 0", "PATCH", "/api/agents/alice", `{"maxConcurrent":0}`},
		{"maxConcurrent 9", "PATCH", "/api/agents/alice", `{"maxConcurrent":9}`},
		{"maxConcurrent float", "PATCH", "/api/agents/alice", `{"maxConcurrent":2.5}`},
		{"create bad name", "POST", "/api/agents", `{"name":"Bob","repo":"owner/x"}`},
		{"create long name", "POST", "/api/agents", `{"name":"` + strings.Repeat("a", 41) + `","repo":"owner/x"}`},
		{"create no repo", "POST", "/api/agents", `{"name":"bob"}`},
		{"create bad repo", "POST", "/api/agents", `{"name":"bob","repo":"x"}`},
		{"create bad forge", "POST", "/api/agents", `{"name":"bob","repo":"owner/x","forge":"gitlab"}`},
		{"create foreign claude secret", "POST", "/api/agents", `{"name":"bob","repo":"owner/x","claudeSecret":"claude-alice"}`},
		{"create forgeSecret not settable", "POST", "/api/agents", `{"name":"bob","repo":"owner/x","forgeSecret":"agent-alice"}`},
		{"create image not settable", "POST", "/api/agents", `{"name":"bob","repo":"owner/x","image":"evil"}`},
		{"delete without keeping state", "DELETE", "/api/agents/alice?keepState=false", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, gitea.URL)
			w := e.call(c.method, c.path, "admin", c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if a := e.lastAudit(t); a.Result != "invalid" {
				t.Errorf("audit: %+v", a)
			}
			if e.spec(t, "alice") == nil || e.spec(t, "bob") != nil {
				t.Error("agents changed on an invalid request")
			}
		})
	}
	e := newEnv(t, gitea.URL)
	if w := e.call("PATCH", "/api/agents/nope", "admin", `{"paused":true}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", w.Code)
	}
	if w := e.call("POST", "/api/agents", "admin", `{"name":"alice","repo":"owner/x"}`); w.Code != http.StatusConflict {
		t.Errorf("existing agent: %d", w.Code)
	}
}

// A merge patch changes only the given fields and keeps the rest of the spec.
func TestMergePatch(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	w := e.call("PATCH", "/api/agents/alice", "writer", `{"maxConcurrent":5}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s := e.spec(t, "alice")
	if s["maxConcurrent"] != int64(5) || s["paused"] != false || s["repo"] != "owner/x" || s["claudeSecret"] != "claude-shared" {
		t.Errorf("spec after patch: %v", s)
	}
	var got agent
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.MaxConcurrent != 5 || got.Name != "alice" {
		t.Errorf("response: %+v", got)
	}
	e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true}`)
	if s := e.spec(t, "alice"); s["maxConcurrent"] != int64(5) || s["paused"] != true {
		t.Errorf("second patch: %v", s)
	}
}

func TestCreateSpec(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	w := e.call("POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x","maxConcurrent":3,"scaleToZero":true,"claudeSecret":"claude-bob"}`)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s := e.spec(t, "bob")
	want := map[string]any{"repo": "owner/x", "forge": "gitea", "forgeSecret": "agent-bob", "claudeSecret": "claude-bob",
		"maxConcurrent": int64(3), "scaleToZero": true}
	if !equalJSON(s, want) {
		t.Errorf("spec %v, want %v", s, want)
	}
}

func TestAuditLine(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true,"maxConcurrent":3}`)
	a := e.lastAudit(t)
	if a.Time != "2026-10-31T12:00:00Z" || a.Action != "patch" || a.Agent != "alice" || a.Result != "ok" {
		t.Errorf("audit head: %+v", a)
	}
	if a.Who.Username != "wendy" || a.Who.ForgeUser != "wendy" || a.Who.Subject != "s-w" {
		t.Errorf("audit who: %+v", a.Who)
	}
	if a.Before == nil || a.Before.Paused || a.Before.MaxConcurrent != 2 || a.Before.Repo != "owner/x" {
		t.Errorf("before: %+v", a.Before)
	}
	if a.After == nil || !a.After.Paused || a.After.MaxConcurrent != 3 {
		t.Errorf("after: %+v", a.After)
	}
	if !strings.Contains(e.audit.String(), `"request":{"maxConcurrent":3,"paused":true}`) {
		t.Errorf("request not logged: %s", e.audit)
	}
	e.call("DELETE", "/api/agents/alice", "admin", "")
	if a := e.lastAudit(t); a.Action != "delete" || a.Before == nil || a.After != nil || a.KeepState == nil || !*a.KeepState {
		t.Errorf("delete audit: %+v", a)
	}
	// the token never appears in the audit line
	if strings.Contains(e.audit.String(), "Bearer") || strings.Contains(e.audit.String(), "svc") {
		t.Error("credentials in the audit log")
	}
}

// CSRF: writes need the Authorization header; cookies are ignored.
func TestWriteNeedsBearer(t *testing.T) {
	e := newEnv(t, "")
	for _, m := range []string{"PATCH /api/agents/alice", "POST /api/agents", "DELETE /api/agents/alice"} {
		method, path, _ := strings.Cut(m, " ")
		r := httptest.NewRequest(method, path, strings.NewReader(`{"paused":true}`))
		r.AddCookie(&http.Cookie{Name: "access_token", Value: "admin"})
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with a cookie only: %d", m, w.Code)
		}
	}
	if e.spec(t, "alice")["paused"] != false {
		t.Error("changed without a bearer token")
	}
}

// The permission of the forge maps to the levels, and unexpected answers fail closed.
func TestGiteaPerms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/boom/"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/garbage/"):
			w.Write([]byte("<html>"))
		case strings.Contains(r.URL.Path, "/owner/"):
			w.Write([]byte(`{"permission":"owner"}`))
		default:
			w.Write([]byte(`{"permission":"none"}`))
		}
	}))
	defer srv.Close()
	g := newGiteaPerms(srv.URL, "t")
	ctx := context.Background()
	for _, c := range []struct {
		user, repo string
		want       perm
		err        bool
	}{
		{"u", "o/owner", permAdmin, false}, {"u", "o/x", permNone, false},
		{"boom", "o/boom", permNone, true}, {"u", "o/garbage", permNone, true},
	} {
		got, err := g.Permission(ctx, "gitea", c.repo, c.user) // path: /repos/o/<name>/collaborators/<user>/permission
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s %s: %v %v", c.user, c.repo, got, err)
		}
		if err != nil && !errors.Is(err, errForgeUnavailable) {
			t.Errorf("error is not errForgeUnavailable: %v", err)
		}
	}
	if _, err := g.Permission(ctx, "github", "o/x", "u"); !errors.Is(err, errForgeUnavailable) {
		t.Error("github must fail closed until implemented")
	}
	if _, err := newGiteaPerms(srv.URL, "").Permission(ctx, "gitea", "o/x", "u"); !errors.Is(err, errForgeUnavailable) {
		t.Error("missing token must fail closed")
	}
}
