package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture: a bare "forge" repo (file://) with main, the sidecar mirror, and an agent clone fed from a sync bundle.
type fixture struct {
	c                 *Config
	g                 *gitRepo
	forge, agent, tmp string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	forge := filepath.Join(tmp, "forge", "owner", "repo.git")
	sh(t, tmp, "init", "-q", "--bare", "-b", "main", forge)
	seed := filepath.Join(tmp, "seed")
	sh(t, tmp, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644)
	os.MkdirAll(filepath.Join(seed, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(seed, ".github", "workflows", "ci.yml"), []byte("on: push\n"), 0o644)
	sh(t, seed, "add", "-A")
	sh(t, seed, "commit", "-qm", "init")
	sh(t, seed, "push", "-q", forge, "main")
	c := &Config{ForgeURL: "file://" + filepath.Join(tmp, "forge"), ForgeUser: "alice", Token: "tok", Repos: []string{"owner/repo"},
		MirrorDir: filepath.Join(tmp, "side", "repo.git"), IncomingDir: filepath.Join(tmp, "side", "incoming"),
		Protected: []string{".github/workflows/", ".gitea/workflows/"}, MaxPushBytes: 1 << 20, Git: "git", Self: "/bin/false"}
	g := &gitRepo{c: c}
	if err := g.init(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{c: c, g: g, forge: forge, agent: filepath.Join(tmp, "agent"), tmp: tmp}
	f.sync(t)
	return f
}

// sync as agent-loop.sh does it: bundle from the sidecar, fetched into the agent's own clone
func (f *fixture) sync(t *testing.T) {
	t.Helper()
	var b bytes.Buffer
	if err := f.g.Sync(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	bf := filepath.Join(f.tmp, "sync.bundle")
	os.WriteFile(bf, b.Bytes(), 0o644)
	if _, err := os.Stat(f.agent); err != nil {
		sh(t, f.tmp, "init", "-q", f.agent)
	}
	sh(t, f.agent, "fetch", "-q", "--prune", bf, "+refs/heads/*:refs/remotes/origin/*")
}

func (f *fixture) commit(t *testing.T, branch, path, content string) {
	t.Helper()
	if sh(t, f.agent, "branch", "--list", branch) == "" {
		sh(t, f.agent, "checkout", "-q", "-B", branch, "origin/main")
	} else {
		sh(t, f.agent, "checkout", "-q", branch)
	}
	os.MkdirAll(filepath.Dir(filepath.Join(f.agent, path)), 0o755)
	os.WriteFile(filepath.Join(f.agent, path), []byte(content), 0o644)
	sh(t, f.agent, "add", "-A")
	sh(t, f.agent, "commit", "-qm", "change "+path)
}

func (f *fixture) bundle(t *testing.T) *bytes.Reader {
	t.Helper()
	bf := filepath.Join(f.tmp, "push.bundle")
	sh(t, f.agent, "bundle", "create", "-q", bf, "HEAD", "--not", "--remotes")
	b, _ := os.ReadFile(bf)
	return bytes.NewReader(b)
}

func isPolicy(err error) bool { var pe *policyError; return errors.As(err, &pe) }

func TestPushOwnBranch(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "agent/alice/issue-1", "src/a.go", "package a\n")
	tip, err := f.g.Push(context.Background(), "agent/alice/issue-1", f.bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := sh(t, f.tmp, "--git-dir", f.forge, "rev-parse", "refs/heads/agent/alice/issue-1"); got != tip {
		t.Fatalf("forge has %s, want %s", got, tip)
	}
	// a second commit on the same branch is a fast-forward
	f.commit(t, "agent/alice/issue-1", "src/b.go", "package a\n")
	if _, err := f.g.Push(context.Background(), "agent/alice/issue-1", f.bundle(t)); err != nil {
		t.Fatal(err)
	}
}

func TestPushRefusesOtherBranches(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "agent/alice/x", "a", "a")
	for _, b := range []string{"main", "agent/bob/x", "agent/alice/", "agent/alice", "agent/alice/../main",
		"agent/alice/x.lock", "agent/alice/.hidden", "refs/heads/agent/alice/x", "agent/alice/a b", "agent/alice/x:main"} {
		if _, err := f.g.Push(context.Background(), b, f.bundle(t)); err == nil || !isPolicy(err) {
			t.Errorf("branch %q: want a policy refusal, got %v", b, err)
		}
	}
	if out := sh(t, f.tmp, "--git-dir", f.forge, "for-each-ref", "--format=%(refname)"); out != "refs/heads/main" {
		t.Fatalf("forge refs changed: %s", out)
	}
}

func TestPushRefusesProtectedPaths(t *testing.T) {
	cases := map[string]func(f *fixture){
		"edit workflow": func(f *fixture) {
			f.commit(t, "agent/alice/w", ".github/workflows/ci.yml", "on: [push, pull_request]\n")
		},
		"new workflow": func(f *fixture) { f.commit(t, "agent/alice/w", ".gitea/workflows/x.yml", "x\n") },
		"add then remove": func(f *fixture) {
			f.commit(t, "agent/alice/w", ".github/workflows/evil.yml", "x\n")
			os.Remove(filepath.Join(f.agent, ".github/workflows/evil.yml"))
			sh(t, f.agent, "commit", "-qam", "rm")
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mk(f)
			_, err := f.g.Push(context.Background(), "agent/alice/w", f.bundle(t))
			if err == nil || !isPolicy(err) || !strings.Contains(err.Error(), "protected path") {
				t.Fatalf("want protected-path refusal, got %v", err)
			}
		})
	}
}

func TestPushRefusesProtectedViaMerge(t *testing.T) {
	f := newFixture(t)
	// someone else's branch on the forge changes a workflow; the agent merges it into its own branch
	other := filepath.Join(f.tmp, "other")
	sh(t, f.tmp, "clone", "-q", f.forge, other)
	sh(t, other, "checkout", "-qb", "feature")
	os.WriteFile(filepath.Join(other, ".github/workflows/ci.yml"), []byte("evil\n"), 0o644)
	sh(t, other, "commit", "-qam", "wf")
	sh(t, other, "push", "-q", "origin", "feature")
	f.sync(t)
	f.commit(t, "agent/alice/m", "a.txt", "a")
	sh(t, f.agent, "merge", "-q", "--no-edit", "origin/feature")
	if _, err := f.g.Push(context.Background(), "agent/alice/m", f.bundle(t)); err == nil || !strings.Contains(err.Error(), "protected path") {
		t.Fatalf("want refusal for a workflow change brought in by a merge, got %v", err)
	}
}

func TestPushSizeLimit(t *testing.T) {
	f := newFixture(t)
	f.c.MaxPushBytes = 1024
	big := make([]byte, 16<<10)
	_, _ = rand.Read(big) // incompressible
	f.commit(t, "agent/alice/big", "big.bin", string(big))
	if _, err := f.g.Push(context.Background(), "agent/alice/big", f.bundle(t)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("want size refusal, got %v", err)
	}
}

func TestPushRefusesForceAndGarbage(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "agent/alice/f", "a", "1")
	if _, err := f.g.Push(context.Background(), "agent/alice/f", f.bundle(t)); err != nil {
		t.Fatal(err)
	}
	// rewrite history: a non-fast-forward must fail at the forge
	sh(t, f.agent, "reset", "-q", "--hard", "origin/main")
	f.commit(t, "agent/alice/f", "b", "2")
	if _, err := f.g.Push(context.Background(), "agent/alice/f", f.bundle(t)); err == nil {
		t.Fatal("non-fast-forward push accepted")
	}
	if _, err := f.g.Push(context.Background(), "agent/alice/f", strings.NewReader("not a bundle")); err == nil || !isPolicy(err) {
		t.Fatalf("garbage accepted: %v", err)
	}
}

func TestMirrorRunsNoHooks(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.tmp, "pwned")
	// even a hook in the mirror itself is never run (core.hooksPath=/dev/null)
	os.WriteFile(filepath.Join(f.c.MirrorDir, "hooks", "reference-transaction"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	f.commit(t, "agent/alice/h", "a", "1")
	if _, err := f.g.Push(context.Background(), "agent/alice/h", f.bundle(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran in the sidecar")
	}
}

func TestCredentialHelper(t *testing.T) {
	t.Setenv("FORGE_URL", "http://gitea.forge:3000/")
	t.Setenv("FORGE_USER", "alice")
	t.Setenv("FORGE_TOKEN", "s3cret")
	ask := func(in string) string {
		var out bytes.Buffer
		credentialHelper([]string{"get"}, strings.NewReader(in), &out)
		return out.String()
	}
	if got := ask("protocol=http\nhost=gitea.forge:3000\n\n"); !strings.Contains(got, "password=s3cret") {
		t.Fatalf("forge host: %q", got)
	}
	for _, in := range []string{"protocol=http\nhost=evil.example\n\n", "protocol=https\nhost=gitea.forge:3000\n\n",
		"protocol=http\nhost=gitea.forge\n\n"} {
		if got := ask(in); got != "" {
			t.Errorf("%q answered: %q", in, got)
		}
	}
	var out bytes.Buffer
	credentialHelper([]string{"store"}, strings.NewReader("protocol=http\nhost=gitea.forge:3000\n\n"), &out)
	if out.Len() != 0 {
		t.Error("store answered")
	}
}

// fake Gitea: records calls and the Authorization header it got
func fakeForge(t *testing.T) (*httptest.Server, *[]string) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":7}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIProxy(t *testing.T) {
	forge, calls := fakeForge(t)
	s := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	h := s.routes()
	ok := []struct{ m, p, b string }{
		{"GET", "/api/v1/user", ""},
		{"GET", "/api/v1/repos/owner/repo", ""},
		{"GET", "/api/v1/repos/owner/repo/issues?state=open&type=issues&assigned_by=alice&limit=50", ""},
		{"GET", "/api/v1/repos/owner/repo/issues/3", ""},
		{"POST", "/api/v1/repos/owner/repo/issues/3/comments", `{"body":"hi"}`},
		{"GET", "/api/v1/repos/owner/repo/issues/3/comments?since=2026-01-01T00:00:00Z", ""},
		{"GET", "/api/v1/repos/owner/repo/pulls?state=all&limit=50", ""},
		{"POST", "/api/v1/repos/owner/repo/pulls", `{"title":"t","head":"agent/alice/issue-3","base":"main","body":"b"}`},
		{"GET", "/api/v1/repos/owner/repo/pulls/7", ""},
		{"GET", "/api/v1/repos/owner/repo/pulls/7/reviews", ""},
		{"GET", "/api/v1/repos/owner/repo/pulls/7/reviews/9/comments", ""},
		{"POST", "/api/v1/repos/owner/repo/pulls/7/requested_reviewers", `{"reviewers":["owner"]}`},
	}
	for _, c := range ok {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != 200 {
			t.Errorf("%s %s: %d %s", c.m, c.p, rec.Code, rec.Body)
		}
	}
	if len(*calls) != len(ok) || !strings.HasSuffix((*calls)[0], "auth=token s3cret") ||
		!strings.Contains((*calls)[2], "assigned_by=alice") {
		t.Fatalf("forge calls: %v", *calls)
	}
	n := len(*calls)
	refused := []struct{ m, p, b string }{
		{"GET", "/api/v1/repos/other/repo/issues", ""},                           // another repo
		{"DELETE", "/api/v1/repos/owner/repo", ""},                               // not on the list
		{"POST", "/api/v1/repos/owner/repo/pulls/7/merge", `{"Do":"merge"}`},     // merging
		{"PATCH", "/api/v1/repos/owner/repo/pulls/7", `{"base":"x"}`},            // editing a PR
		{"POST", "/api/v1/repos/owner/repo/pulls", `{"head":"main","base":"x"}`}, // PR from a branch not its own
		{"GET", "/api/v1/user/repos", ""},
		{"POST", "/api/v1/repos/owner/repo/hooks", `{}`},
		{"GET", "/api/v1/repos/owner/repo/../../admin/users", ""},
	}
	for _, c := range refused {
		if rec := call(t, h, c.m, c.p, c.b); rec.Code != http.StatusForbidden && rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s %s: want refusal, got %d", c.m, c.p, rec.Code)
		}
	}
	if len(*calls) != n {
		t.Fatalf("refused calls reached the forge: %v", (*calls)[n:])
	}
	req := httptest.NewRequest("GET", "/api/v1/user", nil)
	req.RemoteAddr = "10.1.2.3:4444"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback caller: %d", rec.Code)
	}
}

func TestAPISyncAndPush(t *testing.T) {
	f := newFixture(t)
	h := (&server{c: f.c, git: f.g}).routes()
	f.commit(t, "agent/alice/api", "x.txt", "x")
	b, _ := io.ReadAll(f.bundle(t))
	if rec := call(t, h, "POST", "/push?branch=agent/alice/api&base=main", string(b)); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	} else {
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out["sha"]) != 40 {
			t.Fatalf("push answer: %s", rec.Body)
		}
	}
	if rec := call(t, h, "POST", "/push?branch=main&base=main", string(b)); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "not under agent/alice/") {
		t.Fatalf("push to main: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/sync", ""); rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("# v2 git bundle")) {
		t.Fatalf("sync: %d", rec.Code)
	}
}

func TestWebhook(t *testing.T) {
	dir := t.TempDir()
	c := &Config{WakeToken: "wake", WakeFile: filepath.Join(dir, "wake"), ForgeUser: "alice"}
	h := wakeHandler(c, t.Logf)
	req := func(auth, body string) int {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if req("Bearer nope", `{}`) != 401 || req("", `{}`) != 401 {
		t.Fatal("bad token accepted")
	}
	if req("Bearer wake", `{"event":"issues"}`) != 204 || req("Bearer wake", `{"event":"x\ny"}`) != 204 {
		t.Fatal("good token refused")
	}
	b, _ := os.ReadFile(c.WakeFile)
	if l := lines(string(b)); len(l) != 2 || !strings.HasSuffix(l[0], " issues") || !strings.HasSuffix(l[1], " unknown") {
		t.Fatalf("wake file: %q", b)
	}
	// a symlink planted by the agent container is not followed
	os.Remove(c.WakeFile)
	os.Symlink(filepath.Join(dir, "elsewhere"), c.WakeFile)
	if req("Bearer wake", `{}`) != 500 {
		t.Fatal("followed a symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, "elsewhere")); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func TestIdleUsesWakeToken(t *testing.T) {
	var got string
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.URL.Path + " " + r.Header.Get("Authorization") + " " + string(b)
		_, _ = io.WriteString(w, `{"stopped":false,"reason":"recent wake"}`)
	}))
	defer router.Close()
	s := newServer(&Config{ForgeUser: "alice", AgentName: "alice", RouterURL: router.URL, WakeToken: "wake", RegisterToken: "reg", SelfURL: "http://x/"})
	rec := call(t, s.routes(), "POST", "/idle", `{"idleSince":123}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "recent wake") {
		t.Fatalf("idle: %d %s", rec.Code, rec.Body)
	}
	if got != `/idle Bearer wake {"agent":"alice","idleSince":123}` {
		t.Fatalf("router got %q", got)
	}
}

func TestProtectedHit(t *testing.T) {
	p := []string{".github/workflows/", "CODEOWNERS"}
	if protectedHit([]string{"src/a", ".github/x"}, p) != "" {
		t.Error("false positive")
	}
	if protectedHit([]string{".github/workflows/a.yml"}, p) == "" || protectedHit([]string{"CODEOWNERS"}, p) == "" {
		t.Error("miss")
	}
	if protectedHit([]string{"CODEOWNERS.md"}, p) != "" {
		t.Error("exact file matched a prefix")
	}
}

// captureEvents redirects security events for one test and returns them, parsed
func captureEvents(t *testing.T) func() []securityEvent {
	t.Helper()
	var buf bytes.Buffer
	old := eventOut
	eventOut = &buf
	t.Cleanup(func() { eventOut = old })
	return func() []securityEvent {
		var out []securityEvent
		for _, l := range lines(buf.String()) {
			js, ok := strings.CutPrefix(l, eventPrefix)
			if !ok {
				t.Fatalf("event line without prefix: %q", l)
			}
			var e securityEvent
			if err := json.Unmarshal([]byte(js), &e); err != nil {
				t.Fatalf("bad event JSON %q: %v", js, err)
			}
			out = append(out, e)
		}
		return out
	}
}

// review of #68, finding 1: an evil merge (a merge commit whose own tree adds a workflow), pushed through the API
// with no or a bogus base, must still be refused
func TestPushRefusesEvilMergeWithoutBase(t *testing.T) {
	for _, q := range []string{"", "&base=", "&base=does-not-exist", "&base=agent/alice/m"} {
		t.Run(q, func(t *testing.T) {
			f := newFixture(t)
			f.c.AgentName = "alice"
			events := captureEvents(t)
			h := (&server{c: f.c, git: f.g}).routes()
			f.commit(t, "agent/alice/m", "a.txt", "a")
			sh(t, f.agent, "checkout", "-qb", "side", "origin/main")
			os.WriteFile(filepath.Join(f.agent, "b.txt"), []byte("b"), 0o644)
			sh(t, f.agent, "add", "-A")
			sh(t, f.agent, "commit", "-qm", "side")
			sh(t, f.agent, "checkout", "-q", "agent/alice/m")
			sh(t, f.agent, "merge", "-q", "--no-commit", "side")
			os.WriteFile(filepath.Join(f.agent, ".github/workflows/evil.yml"), []byte("evil\n"), 0o644)
			sh(t, f.agent, "add", "-A")
			sh(t, f.agent, "commit", "-qm", "merge")
			b, _ := io.ReadAll(f.bundle(t))
			rec := call(t, h, "POST", "/push?branch=agent/alice/m"+q, string(b))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "protected path") {
				t.Fatalf("want protected-path refusal, got %d %s", rec.Code, rec.Body)
			}
			if ev := events(); len(ev) != 1 || ev[0].Rule != "push-protected-path" || ev[0].Severity != "high" {
				t.Fatalf("events: %+v", ev)
			}
		})
	}
}

func TestSecurityEvents(t *testing.T) {
	want := func(t *testing.T, ev []securityEvent, rule, sev string) {
		t.Helper()
		if len(ev) != 1 || ev[0].Source != "forge" || ev[0].Rule != rule || ev[0].Severity != sev || ev[0].Agent != "alice" || ev[0].Action == "" {
			t.Fatalf("want one %s/%s event, got %+v", rule, sev, ev)
		}
	}
	t.Run("push outside own branches", func(t *testing.T) {
		f := newFixture(t)
		f.c.AgentName = "alice"
		events := captureEvents(t)
		h := (&server{c: f.c, git: f.g}).routes()
		f.commit(t, "agent/alice/x", "a", "a")
		b, _ := io.ReadAll(f.bundle(t))
		req := httptest.NewRequest("POST", "/push?branch=main", bytes.NewReader(b))
		req.RemoteAddr, req.Header = "127.0.0.1:1", http.Header{"X-Agentdev-Job": {"issue-12"}}
		h.ServeHTTP(httptest.NewRecorder(), req)
		ev := events()
		want(t, ev, "push-foreign-branch", "high")
		if ev[0].Job != "issue-12" || !strings.Contains(ev[0].Action, "push to main") {
			t.Fatalf("job/action: %+v", ev[0])
		}
	})
	t.Run("oversize push", func(t *testing.T) {
		f := newFixture(t)
		f.c.AgentName, f.c.MaxPushBytes = "alice", 1024
		events := captureEvents(t)
		big := make([]byte, 16<<10)
		_, _ = rand.Read(big)
		f.commit(t, "agent/alice/big", "big.bin", string(big))
		b, _ := io.ReadAll(f.bundle(t))
		call(t, (&server{c: f.c, git: f.g}).routes(), "POST", "/push?branch=agent/alice/big", string(b))
		want(t, events(), "push-oversize", "medium")
	})
	forge, _ := fakeForge(t)
	s := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", AgentName: "alice", Token: "t", Repos: []string{"owner/repo"}})
	for _, c := range []struct{ name, m, p, b, rule, sev string }{
		{"call outside the allow-list", "POST", "/api/v1/repos/owner/repo/pulls/1/merge", "{}", "api-not-allowed", "medium"},
		{"another repo", "GET", "/api/v1/repos/evil/repo/issues", "", "api-other-repo", "high"},
		{"PR from a foreign branch", "POST", "/api/v1/repos/owner/repo/pulls", `{"head":"main"}`, "push-foreign-branch", "high"},
	} {
		t.Run(c.name, func(t *testing.T) {
			events := captureEvents(t)
			call(t, s.routes(), c.m, c.p, c.b)
			want(t, events(), c.rule, c.sev)
		})
	}
	t.Run("caller outside the pod", func(t *testing.T) {
		events := captureEvents(t)
		req := httptest.NewRequest("GET", "/api/v1/user", nil)
		req.RemoteAddr = "10.0.0.9:1"
		s.routes().ServeHTTP(httptest.NewRecorder(), req)
		want(t, events(), "api-non-loopback", "high")
	})
	t.Run("bad webhook token", func(t *testing.T) {
		events := captureEvents(t)
		h := wakeHandler(&Config{WakeToken: "w", AgentName: "alice", WakeFile: filepath.Join(t.TempDir(), "w")}, t.Logf)
		r := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer bad")
		h.ServeHTTP(httptest.NewRecorder(), r)
		want(t, events(), "webhook-bad-token", "medium")
	})
	t.Run("allowed calls emit nothing; job header is validated", func(t *testing.T) {
		events := captureEvents(t)
		call(t, s.routes(), "GET", "/api/v1/repos/owner/repo", "")
		emitEvent("alice", "issue-1\n{evil}", "r", "low", "line1\nline2")
		ev := events()
		if len(ev) != 1 || ev[0].Job != "" || ev[0].Action != "line1\nline2" {
			t.Fatalf("got %+v", ev)
		}
	})
}

// review #102: the sidecar is the only usage channel; it checks the schema, the ranges and the rate
func TestUsageReport(t *testing.T) {
	var out, ev bytes.Buffer
	oldU, oldE := usageOut, eventOut
	usageOut, eventOut = &out, &ev
	t.Cleanup(func() { usageOut, eventOut = oldU, oldE })
	s := newServer(&Config{ForgeUser: "alice", AgentName: "alice"})
	h := s.routes()
	reset := time.Now().Add(2 * time.Hour).Unix()
	good := fmt.Sprintf(`{"job":"issue-1","input_tokens":5,"output_tokens":2,"rate_limited":false,"resets_at":0,"five_hour":{"utilization":0.4,"resetsAt":%d},"seven_day":null}`, reset)
	if rec := call(t, h, "POST", "/usage", good); rec.Code != 204 || !strings.HasPrefix(out.String(), "AGENTDEV_USAGE {") {
		t.Fatalf("good report: %d %q", rec.Code, out.String())
	}
	for name, body := range map[string]string{
		"unknown field":        `{"job":"issue-1","extra":1}`,
		"not json":             `AGENTDEV_USAGE {}`,
		"negative tokens":      `{"input_tokens":-1}`,
		"reset beyond window":  fmt.Sprintf(`{"five_hour":{"utilization":0.9,"resetsAt":%d}}`, time.Now().Add(100*time.Hour).Unix()),
		"resets_at far future": `{"rate_limited":true,"resets_at":99999999999}`,
		"string reset":         `{"resets_at":"a[$(touch /tmp/pwned)]"}`,
	} {
		out.Reset()
		if rec := call(t, h, "POST", "/usage", body); rec.Code != 400 || out.Len() != 0 {
			t.Fatalf("%s: want 400 and no line, got %d %q", name, rec.Code, out.String())
		}
	}
	if !strings.Contains(ev.String(), "usage-report-invalid") {
		t.Fatalf("no security event for an invalid report: %s", ev.String())
	}
	for i := 0; i < usageMaxPerMinute+3; i++ {
		call(t, h, "POST", "/usage", good)
	}
	if n := strings.Count(out.String(), "AGENTDEV_USAGE"); n != usageMaxPerMinute-1 {
		t.Fatalf("rate limit: %d lines printed, want %d", n, usageMaxPerMinute-1)
	}
	if strings.Count(ev.String(), "usage-report-flood") != 1 {
		t.Fatalf("one flood event per burst: %s", ev.String())
	}
}
