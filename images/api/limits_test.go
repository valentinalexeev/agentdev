package main

import (
	"context"
	"strings"
	"testing"
)

// #27 acceptance: an admin sets a limit of 2 agents, and a user cannot launch more than that.
func TestCreateTotalLimit(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL) // alice exists already (made outside the API): counts for the total
	e.wr.limits = agentLimits{Total: 2}
	if w := e.call("POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x"}`); w.Code != 201 {
		t.Fatalf("second agent: %d %s", w.Code, w.Body)
	}
	w := e.call("POST", "/api/agents", "admin", `{"name":"carl","repo":"owner/x"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "instance limit of 2 agents") {
		t.Fatalf("third agent: %d %s", w.Code, w.Body)
	}
	if e.spec(t, "carl") != nil {
		t.Error("carl was created above the limit")
	}
	if a := e.lastAudit(t); a.Result != "denied" || a.Action != "create" || a.Agent != "carl" {
		t.Errorf("audit %+v", a)
	}
	// the admin group is limited by the instance cap too
	if w := e.call("POST", "/api/agents", "group", `{"name":"dora","repo":"owner/x"}`); w.Code != 403 {
		t.Errorf("admin group above the total: %d", w.Code)
	}
	// after a delete there is room again
	if w := e.call("DELETE", "/api/agents/bob", "admin", ""); w.Code != 200 && w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := e.call("POST", "/api/agents", "admin", `{"name":"carl","repo":"owner/x"}`); w.Code != 201 {
		t.Errorf("after delete: %d %s", w.Code, w.Body)
	}
}

func TestCreatePerUserLimit(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	e.wr.limits = agentLimits{PerUser: 1}
	if w := e.call("POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x"}`); w.Code != 201 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	u, _ := e.k.Get(context.Background(), "bob")
	if got := u.GetAnnotations()[createdByAnnotation]; got != "s-a" {
		t.Errorf("created-by %q, want the OIDC subject s-a", got)
	}
	w := e.call("POST", "/api/agents", "admin", `{"name":"carl","repo":"owner/x"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "your limit of 1 agents") {
		t.Fatalf("second for adam: %d %s", w.Code, w.Body)
	}
	// alice (no annotation, made by Ansible) does not count for anyone; the admin group is not limited per user
	for _, n := range []string{"dora", "eve"} {
		if w := e.call("POST", "/api/agents", "group", `{"name":"`+n+`","repo":"owner/x"}`); w.Code != 201 {
			t.Errorf("admin group %s: %d %s", n, w.Code, w.Body)
		}
	}
}

func TestCreateNoLimitByDefault(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	for _, n := range []string{"b1", "b2", "b3", "b4"} {
		if w := e.call("POST", "/api/agents", "admin", `{"name":"`+n+`","repo":"owner/x"}`); w.Code != 201 {
			t.Fatalf("%s: %d %s", n, w.Code, w.Body)
		}
	}
}

func TestEnvCount(t *testing.T) {
	for v, want := range map[string]int{"": 0, "0": 0, "2": 2, " 10 ": 10} {
		t.Setenv("X_CAP", v)
		if got := envCount("X_CAP"); got != want {
			t.Errorf("%q: %d, want %d", v, got, want)
		}
	}
}

// #27 acceptance, word for word: the admin sets a per-user limit of 2 agents. A user (adam, not in
// agentdev-admins) launches two agents, and the third launch is refused and not created.
func TestAcceptanceUserLimitOfTwo(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	e.wr.limits = agentLimits{PerUser: 2}
	for _, n := range []string{"bob", "carl"} {
		if w := e.call("POST", "/api/agents", "admin", `{"name":"`+n+`","repo":"owner/x"}`); w.Code != 201 {
			t.Fatalf("%s within the limit: %d %s", n, w.Code, w.Body)
		}
	}
	w := e.call("POST", "/api/agents", "admin", `{"name":"dora","repo":"owner/x"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "your limit of 2 agents") {
		t.Fatalf("third agent: %d %s", w.Code, w.Body)
	}
	if e.spec(t, "dora") != nil {
		t.Error("dora was created above the limit")
	}
	if a := e.lastAudit(t); a.Result != "denied" || a.Agent != "dora" {
		t.Errorf("audit %+v", a)
	}
}
