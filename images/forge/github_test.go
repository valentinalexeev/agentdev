package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ghFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeGitHub replays the recorded answers and records every call (method, path, auth, body).
type ghCall struct{ method, path, auth, body string }

func fakeGitHub(t *testing.T) (*httptest.Server, func() []ghCall) {
	var mu sync.Mutex
	var calls []ghCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, ghCall{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/octo-org/repo/installation":
			w.Write(ghFixture(t, "installation.json"))
		case r.Method == "POST" && r.URL.Path == "/app/installations/51234567/access_tokens":
			w.WriteHeader(201)
			w.Write(ghFixture(t, "access_token.json"))
		case r.Method == "GET" && r.URL.Path == "/app":
			w.Write(ghFixture(t, "app.json"))
		case r.Method == "GET" && r.URL.Path == "/users/my-agentdev[bot]":
			w.Write(ghFixture(t, "bot_user.json"))
		case r.Method == "GET" && r.URL.Path == "/repos/octo-org/repo/issues":
			w.Write(ghFixture(t, "issues_labeled.json"))
		case r.Method == "POST" && r.URL.Path == "/repos/octo-org/repo/pulls":
			w.WriteHeader(201)
			w.Write(ghFixture(t, "pull_created.json"))
		case r.Method == "GET" && r.URL.Path == "/repos/octo-org/repo/pulls/14/comments":
			w.Write(ghFixture(t, "review_comments.json"))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []ghCall { mu.Lock(); defer mu.Unlock(); return append([]ghCall(nil), calls...) }
}

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func ghConfig(t *testing.T, api string) (*Config, *rsa.PrivateKey) {
	k, p := testKey(t)
	t.Setenv("GITHUB_API_URL", api)
	return &Config{ForgeURL: "https://github.com", ForgeUser: "alice", AgentName: "alice", ForgeKind: "github",
		Repos: []string{"octo-org/repo"}, GitHubAppID: "1012345", GitHubAppKey: p}, k
}

// fixed clock just before the recorded token's expiry window
var ghNow = time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)

func newTestGitHub(t *testing.T) (*githubDriver, *rsa.PrivateKey, func() []ghCall) {
	srv, calls := fakeGitHub(t)
	c, k := ghConfig(t, srv.URL)
	d, err := newGitHubDriver(c)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return ghNow.Add(-30 * time.Minute) }
	return d, k, calls
}

func TestGitHubDriverConfigValidation(t *testing.T) {
	_, p := testKey(t)
	base := func() *Config {
		return &Config{ForgeURL: "https://github.com", ForgeUser: "alice", AgentName: "alice", ForgeKind: "github",
			Repos: []string{"octo-org/repo"}, GitHubAppID: "1012345", GitHubAppKey: p}
	}
	if _, err := newGitHubDriver(base()); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"app id":       func(c *Config) { c.GitHubAppID = "12; rm -rf /" },
		"app id zero":  func(c *Config) { c.GitHubAppID = "0" },
		"install id":   func(c *Config) { c.GitHubInstallationID = "-1" },
		"key":          func(c *Config) { c.GitHubAppKey = "not a key" },
		"agent":        func(c *Config) { c.ForgeUser = "Alice/../x" },
		"agent name":   func(c *Config) { c.AgentName = "a b" },
		"repo":         func(c *Config) { c.Repos = []string{"octo-org"} },
		"two owners":   func(c *Config) { c.Repos = []string{"a/x", "b/y"} },
		"repo owner":   func(c *Config) { c.Repos = []string{"-bad/x"} },
		"forge url":    func(c *Config) { c.ForgeURL = "::" },
		"unknown kind": func(c *Config) { c.ForgeKind = "gitlab" },
	}
	for name, mut := range bad {
		c := base()
		mut(c)
		if _, err := newDriver(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("GITHUB_API_URL", "https://user:pw@evil.example")
	if _, err := newGitHubDriver(base()); err == nil {
		t.Error("API URL with credentials accepted")
	}
	t.Setenv("GITHUB_API_URL", "")
	if d, _ := newGitHubDriver(base()); d.api != "https://api.github.com" {
		t.Errorf("github.com api: %s", d.api)
	}
	c := base()
	c.ForgeURL = "https://ghe.example.com"
	if d, _ := newGitHubDriver(c); d.api != "https://ghe.example.com/api/v3" {
		t.Errorf("GHES api: %s", d.api)
	}
}

func TestGitHubAppJWT(t *testing.T) {
	d, k, _ := newTestGitHub(t)
	jwt, err := d.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt: %s", jwt)
	}
	dec := base64.RawURLEncoding.DecodeString
	h, _ := dec(parts[0])
	cl, _ := dec(parts[1])
	sig, _ := dec(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var hdr map[string]string
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	_ = json.Unmarshal(h, &hdr)
	_ = json.Unmarshal(cl, &claims)
	now := d.now().Unix()
	if hdr["alg"] != "RS256" || claims.Iss != "1012345" || claims.Iat != now-60 || claims.Exp != now+540 {
		t.Fatalf("jwt header %v claims %+v", hdr, claims)
	}
}

func TestGitHubInstallationToken(t *testing.T) {
	d, _, calls := newTestGitHub(t)
	ctx := t.Context()
	tok, err := d.installationToken(ctx)
	if err != nil || tok != "ghs_TESTTOKEN0000000000000000000000000000" {
		t.Fatalf("token %q %v", tok, err)
	}
	cs := calls()
	if len(cs) != 2 || cs[0].path != "/repos/octo-org/repo/installation" || cs[1].path != "/app/installations/51234567/access_tokens" {
		t.Fatalf("calls: %+v", cs)
	}
	for _, c := range cs {
		if !strings.HasPrefix(c.auth, "Bearer ey") {
			t.Errorf("%s not with the App JWT: %q", c.path, c.auth)
		}
	}
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(cs[1].body), &req)
	if len(req.Repositories) != 1 || req.Repositories[0] != "repo" || req.Permissions["contents"] != "write" ||
		req.Permissions["workflows"] != "" || req.Permissions["administration"] != "" || len(req.Permissions) != 6 {
		t.Fatalf("token request not narrowed: %s", cs[1].body)
	}
	// cached
	if _, err := d.installationToken(ctx); err != nil || len(calls()) != 2 {
		t.Fatalf("not cached: %d calls", len(calls()))
	}
	// renewed 10 minutes before expiry (12:00)
	d.now = func() time.Time { return ghNow.Add(51 * time.Minute) }
	_, _ = d.installationToken(ctx)
	if len(calls()) != 3 || calls()[2].path != "/app/installations/51234567/access_tokens" {
		t.Fatalf("not renewed: %+v", calls())
	}
	// GitHub answering with an already expired token is refused
	d.now = func() time.Time { return ghNow.Add(61 * time.Minute) }
	if _, err := d.installationToken(ctx); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestGitHubTokenBadAnswers(t *testing.T) {
	for name, answer := range map[string]string{
		"no token":    `{"expires_at":"2026-11-01T12:00:00Z"}`,
		"bad token":   `{"token":"ghs_x\nInjected: 1","expires_at":"2026-11-01T12:00:00Z"}`,
		"bad expiry":  `{"token":"ghs_TESTTOKEN0000000000000000000000000000","expires_at":"soon"}`,
		"bad inst id": `{"id":"1; DROP"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/installation") && name != "bad inst id" {
				w.Write(ghFixture(t, "installation.json"))
				return
			}
			w.Write([]byte(answer))
		}))
		c, _ := ghConfig(t, srv.URL)
		d, _ := newGitHubDriver(c)
		d.now = func() time.Time { return ghNow }
		if tok, err := d.installationToken(t.Context()); err == nil {
			t.Errorf("%s: accepted %q", name, tok)
		}
		srv.Close()
	}
}

func TestGitHubProxy(t *testing.T) {
	d, _, calls := newTestGitHub(t)
	s := newServer(d.c)
	s.drv, s.rules = d, d.rules()
	h := s.routes()
	ok := []struct{ m, p, b string }{
		{"GET", "/api/v1/repos/octo-org/repo", ""},
		{"GET", "/api/v1/repos/octo-org/repo/issues?state=open&labels=agent:alice&per_page=50", ""},
		{"GET", "/api/v1/repos/octo-org/repo/issues/12", ""},
		{"POST", "/api/v1/repos/octo-org/repo/issues/12/comments", `{"body":"picked up"}`},
		{"POST", "/api/v1/repos/octo-org/repo/pulls", `{"title":"t","head":"agent/alice/issue-12","base":"main","body":"Closes #12"}`},
		{"GET", "/api/v1/repos/octo-org/repo/pulls/14/comments", ""},
		{"POST", "/api/v1/repos/octo-org/repo/pulls/14/comments/1900000001/replies", `{"body":"Fixed in 0123456"}`},
		{"POST", "/api/v1/repos/octo-org/repo/pulls/14/requested_reviewers", `{"reviewers":["octo-dev"]}`},
		{"GET", "/api/v1/repos/octo-org/repo/commits/0123456789abcdef0123456789abcdef01234567/check-runs", ""},
	}
	for _, c := range ok {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code/100 != 2 {
			t.Errorf("%s %s: %d %s", c.m, c.p, rec.Code, rec.Body)
		}
	}
	cs := calls()[2:] // after installation lookup and token
	if len(cs) != len(ok) {
		t.Fatalf("calls: %+v", cs)
	}
	for _, c := range cs {
		if c.auth != "Bearer ghs_TESTTOKEN0000000000000000000000000000" {
			t.Errorf("%s %s auth %q", c.method, c.path, c.auth)
		}
	}
	if cs[1].path != "/repos/octo-org/repo/issues?state=open&labels=agent:alice&per_page=50" {
		t.Errorf("query: %s", cs[1].path)
	}
	for _, i := range []int{3, 4, 6} {
		if !strings.Contains(cs[i].body, "agent `alice` (agentdev)") {
			t.Errorf("%s: no signature: %s", cs[i].path, cs[i].body)
		}
	}
	if strings.Contains(cs[7].body, "agentdev") {
		t.Errorf("review request was rewritten: %s", cs[7].body)
	}
	n := len(calls())
	refused := []struct{ m, p, b string }{
		{"GET", "/api/v1/user", ""}, // an installation has no user
		{"GET", "/api/v1/repos/other/repo/issues", ""},
		{"PUT", "/api/v1/repos/octo-org/repo/pulls/14/merge", `{}`},
		{"POST", "/api/v1/repos/octo-org/repo/pulls", `{"head":"octo-dev:agent/alice/x","base":"main","body":"b"}`},
		{"POST", "/api/v1/repos/octo-org/repo/pulls", `{"head":"agent/alice/x","head_repo":"x/y","base":"main","body":"b"}`},
		{"POST", "/api/v1/repos/octo-org/repo/pulls", `{"head":"agent/bob/x","base":"main","body":"b"}`},
		{"POST", "/api/v1/repos/octo-org/repo/issues/12/comments", `{"body":7}`},
		{"POST", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["agent:bob"]}`},
		{"PATCH", "/api/v1/repos/octo-org/repo/issues/12", `{"state":"closed"}`},
		{"POST", "/api/v1/graphql", `{"query":"mutation{resolveReviewThread}"}`},
		{"GET", "/api/v1/repos/octo-org/repo/commits/main/check-runs", ""},
		{"GET", "/api/v1/app/installations", ""},
	}
	for _, c := range refused {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s %s: want 403, got %d", c.m, c.p, c.b, rec.Code)
		}
	}
	if len(calls()) != n {
		t.Fatalf("refused calls reached GitHub: %+v", calls()[n:])
	}
}

func TestGitHubIdentity(t *testing.T) {
	d, _, _ := newTestGitHub(t)
	s := newServer(d.c)
	s.drv = d
	rec := call(t, s.routes(), "GET", "/identity", "")
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out["forge"] != "github" || out["name"] != "alice (agentdev)" ||
		out["email"] != "201234567+my-agentdev[bot]@users.noreply.github.com" || out["login"] != "my-agentdev[bot]" {
		t.Fatalf("identity: %d %s", rec.Code, rec.Body)
	}
	g := newServer(&Config{ForgeURL: "http://gitea:3000", ForgeUser: "alice", AgentName: "alice", Token: "x", Repos: []string{"o/r"}})
	rec = call(t, g.routes(), "GET", "/identity", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["forge"] != "gitea" || out["name"] != "alice" || out["email"] != "alice@agents.local" {
		t.Fatalf("gitea identity: %s", rec.Body)
	}
}

func TestGitHubGitCredential(t *testing.T) {
	d, _, _ := newTestGitHub(t)
	u, p, err := d.gitCredential(t.Context())
	if err != nil || u != "x-access-token" || p != "ghs_TESTTOKEN0000000000000000000000000000" {
		t.Fatalf("%s %s %v", u, p, err)
	}
	if d.remoteURL("octo-org/repo") != "https://github.com/octo-org/repo.git" {
		t.Fatal(d.remoteURL("octo-org/repo"))
	}
}
