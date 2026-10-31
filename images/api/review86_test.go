package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestForgeUserOfRejectsAmbiguousNames(t *testing.T) {
	for in, want := range map[string]string{
		"adam": "adam", "a.b-c_d": "a.b-c_d", "Adam": "", "ADAM": "", " adam": "", "adam ": "", "": "",
		"-adam": "", ".adam": "", "ad/am": "", "adäm": "", strings.Repeat("a", 41): "",
	} {
		if got := forgeUserOf(&identity{Username: in}); got != want {
			t.Errorf("forgeUserOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUppercaseUsernameCannotActAsForgeUser(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	testIDs["Adam"] = &identity{Subject: "s-x", Username: "Adam"}
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
	h := routes(idVerify, e.k, &writer{store: rs, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"})
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
