package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestForgeUserOfRejectsAmbiguousNames(t *testing.T) {
	for in, want := range map[string]string{
		"adam": "adam", "a.b-c_d": "a.b-c_d", "Adam": "", "ADAM": "", " adam": "", "adam ": "", "": "",
		"-adam": "", ".adam": "", "ad/am": "", "adäm": "", strings.Repeat("a", 41): "",
	} {
		if got := forgeUserOf(&identity{ForgeLogin: in}, "gitea"); got != want {
			t.Errorf("forgeUserOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUppercaseUsernameCannotActAsForgeUser(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	testIDs["Adam"] = &identity{Subject: "s-x", Username: "adam", ForgeLogin: "Adam"}
	defer delete(testIDs, "Adam")
	e := newEnv(t, gitea.URL)
	if w := e.call("DELETE", "/api/agents/alice", "Adam", ""); w.Code != http.StatusForbidden {
		t.Fatalf("code %d", w.Code)
	}
	if e.spec(t, "alice") == nil {
		t.Fatal("agent deleted")
	}
}

func TestValidRepo(t *testing.T) {
	for in, want := range map[string]bool{
		"owner/x": true, "o_1/x.y-z": true, "../..": false, "./x": false, "o/..": false, "o/.": false,
		"o/x.": false, "o./x": false, "-o/x": false, "o/-x": false, "o/a..b": false, "o": false, "o/x/y": false, "/x": false,
	} {
		if validRepo(in) != want {
			t.Errorf("validRepo(%q) != %v", in, want)
		}
	}
}

type recStore struct {
	agentStore
	uid, rv string
}

func (s *recStore) Delete(ctx context.Context, name, uid, rv string) error {
	s.uid, s.rv = uid, rv
	return s.agentStore.Delete(ctx, name, uid, rv)
}

func TestDeleteUsesPreconditions(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	u, _ := e.k.Get(context.Background(), "alice")
	u.SetUID("uid-1")
	u.SetResourceVersion("42")
	_ = e.k.d.Resource(agentsGVR).Namespace("coding-agents").Delete(context.Background(), "alice", deleteNoPre)
	_, _ = e.k.Create(context.Background(), u)
	rs := &recStore{agentStore: e.k}
	h := routes(idVerify, e.k, nil, &writer{store: rs, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"})
	e.h = h
	if w := e.call("DELETE", "/api/agents/alice", "admin", ""); w.Code != http.StatusNoContent {
		t.Fatalf("code %d %s", w.Code, w.Body)
	}
	if rs.uid != "uid-1" || rs.rv == "" {
		t.Fatalf("preconditions not passed: uid=%q rv=%q", rs.uid, rs.rv)
	}
}

func TestUnauthenticatedWritesAreAudited(t *testing.T) {
	e := newEnv(t, "")
	for _, tok := range []string{"", "bad-secret-token"} {
		e.audit.Reset()
		if w := e.call("PATCH", "/api/agents/alice", tok, `{"paused":true}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", w.Code)
		}
		a := e.lastAudit(t)
		if a.Result != "unauthenticated" || a.Action != "patch" || a.Agent != "alice" {
			t.Fatalf("audit %+v", a)
		}
		if strings.Contains(e.audit.String(), "bad-secret-token") {
			t.Fatal("token in audit")
		}
	}
	e.audit.Reset()
	e.call("GET", "/api/agents", "", "")
	if e.audit.Len() != 0 {
		t.Fatal("reads must not be audited")
	}
}

func TestGitHubAgentGives403ForRepoUsers(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	u, _ := e.k.Get(context.Background(), "alice")
	_ = unstructured.SetNestedField(u.Object, "github", "spec", "forge")
	_, _ = e.k.d.Resource(agentsGVR).Namespace("coding-agents").Update(context.Background(), u, updateOpts)
	w := e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "issues/87") {
		t.Fatalf("code %d %s", w.Code, w.Body)
	}
	w = e.call("POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x","forge":"github"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("create code %d", w.Code)
	}
	if w = e.call("PATCH", "/api/agents/alice", "group", `{"paused":true}`); w.Code != http.StatusOK {
		t.Fatalf("admin group pause code %d", w.Code)
	}
}

var (
	deleteNoPre = metav1.DeleteOptions{}
	updateOpts  = metav1.UpdateOptions{}
)

// goneStore simulates a concurrent DELETE: the object disappears between the check and the delete.
type goneStore struct{ agentStore }

func (s *goneStore) Delete(context.Context, string, string, string) error {
	return apierrors.NewNotFound(agentsGVR.GroupResource(), "alice")
}

func TestConcurrentDeleteIsNoContent(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	e.h = routes(idVerify, e.k, nil, &writer{store: &goneStore{agentStore: e.k}, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"})
	if w := e.call("DELETE", "/api/agents/alice", "admin", ""); w.Code != http.StatusNoContent {
		t.Fatalf("a delete that lost the race must still answer 204, got %d %s", w.Code, w.Body)
	}
}

func TestClientAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/agents", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	if got := clientAddr(r); got != "10.0.0.5:1234" {
		t.Errorf("no proxy: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := clientAddr(r); got != "203.0.113.7 via 10.0.0.5:1234" {
		t.Errorf("behind proxy: %q", got)
	}
	r.Header.Set("X-Forwarded-For", strings.Repeat("x", 500))
	if got := clientAddr(r); len(got) > 64+len(" via 10.0.0.5:1234") {
		t.Errorf("forwarded value not clipped: %d", len(got))
	}
}

// #88: the authentik username never maps to a forge user; only the forge_login claim does.
func TestUsernameWithoutForgeLoginIsReadOnly(t *testing.T) {
	if got := forgeUserOf(&identity{Username: "adam", Email: "adam@example.com"}, "gitea"); got != "" {
		t.Fatalf("forgeUserOf without claim = %q, want empty", got)
	}
	if got := forgeUserOf(&identity{Username: "mallory", ForgeLogin: "adam"}, "gitea"); got != "adam" {
		t.Fatalf("forgeUserOf = %q, want claim value", got)
	}
	gitea := fakeGitea(t)
	defer gitea.Close()
	testIDs["noclaim"] = &identity{Subject: "s-nc", Username: "adam"} // same username as the repo admin, no claim
	defer delete(testIDs, "noclaim")
	e := newEnv(t, gitea.URL)
	if w := e.call("DELETE", "/api/agents/alice", "noclaim", ""); w.Code != http.StatusForbidden {
		t.Fatalf("code %d", w.Code)
	}
	if e.spec(t, "alice") == nil {
		t.Fatal("agent deleted")
	}
}

// #122: per-forge claims forge_login_<kind> win for their forge, forge_login is the fallback, the claim of another
// forge is never used, and nothing (not the username) fills a missing login.
func TestForgeUserOfPerForgeClaims(t *testing.T) {
	both := &identity{Username: "mallory", ForgeLogin: "fallback", ForgeLoginGitHub: "gh-alice", ForgeLoginGitea: "alice"}
	for _, c := range []struct {
		name  string
		id    *identity
		forge string
		want  string
	}{
		{"github claim", both, "github", "gh-alice"},
		{"gitea claim", both, "gitea", "alice"},
		{"empty forge is gitea", both, "", "alice"},
		{"fallback on github", &identity{ForgeLogin: "fallback", ForgeLoginGitea: "alice"}, "github", "fallback"},
		{"fallback on gitea", &identity{ForgeLogin: "fallback", ForgeLoginGitHub: "gh-alice"}, "gitea", "fallback"},
		{"wrong-forge claim ignored (gitea)", &identity{Username: "alice", ForgeLoginGitHub: "gh-alice"}, "gitea", ""},
		{"wrong-forge claim ignored (github)", &identity{Username: "alice", ForgeLoginGitea: "alice"}, "github", ""},
		{"both missing refused", &identity{Username: "alice", Email: "alice@example.com"}, "github", ""},
		{"invalid per-forge claim refused, no fallback", &identity{ForgeLogin: "fallback", ForgeLoginGitea: "Alice"}, "gitea", ""},
		{"unknown forge uses fallback only", both, "gitlab", "fallback"},
	} {
		if got := forgeUserOf(c.id, c.forge); got != c.want {
			t.Errorf("%s: forgeUserOf(%s) = %q, want %q", c.name, c.forge, got, c.want)
		}
	}
}

// #122: the gitea permission check uses the gitea claim, not the github one.
func TestRepoPermUsesClaimOfAgentForge(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	w := &writer{perms: newGiteaPerms(gitea.URL, "svc")}
	id := &identity{Username: "adam", ForgeLoginGitHub: "adam"}
	if p, err := w.repoPerm(context.Background(), id, "gitea", "owner/x"); err != nil || p != permNone {
		t.Fatalf("github claim used on gitea: %v %v", p, err)
	}
	id = &identity{Username: "mallory", ForgeLogin: "rita", ForgeLoginGitea: "adam", ForgeLoginGitHub: "wendy"}
	if p, err := w.repoPerm(context.Background(), id, "gitea", "owner/x"); err != nil || p != permAdmin {
		t.Fatalf("gitea claim not used: %v %v", p, err)
	}
}
