package main

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func gitEnv(t *testing.T, forgeURL string) *tenv {
	t.Helper()
	e := newEnv(t, forgeURL)
	e.wr.source, e.k.source = agentsSourceGit, agentsSourceGit
	return e
}

func TestAgentsSourceFromEnv(t *testing.T) {
	for in, want := range map[string]string{"": "api", "api": "api", "git": "git", " git ": "git"} {
		got, err := agentsSourceFromEnv(func(string) string { return in })
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := agentsSourceFromEnv(func(string) string { return "flux" }); err == nil {
		t.Error("unknown AGENTS_SOURCE accepted")
	}
}

// git mode: a Git-owned Agent (alice, no agentdev/created-by) is read-only except the runtime field paused.
func TestGitOwnedAgentIsReadOnly(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := gitEnv(t, gitea.URL)
	for _, body := range []string{`{"maxConcurrent":4}`, `{"scaleToZero":false}`, `{"paused":true,"maxConcurrent":3}`} {
		w := e.call("PATCH", "/api/agents/alice", "admin", body)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "owned by Git") {
			t.Errorf("%s: %d %s, want 409 owned by Git", body, w.Code, w.Body)
		}
		if a := e.lastAudit(t); a.Result != "denied" || a.Action != "patch" {
			t.Errorf("%s: audit %+v", body, a)
		}
	}
	if s := e.spec(t, "alice"); s["maxConcurrent"] != int64(2) || s["paused"] != false {
		t.Errorf("Git-owned spec changed: %v", s)
	}
	if w := e.call("DELETE", "/api/agents/alice", "group", ""); w.Code != 409 {
		t.Errorf("delete of a Git-owned agent: %d, want 409", w.Code)
	}
	if e.spec(t, "alice") == nil {
		t.Error("Git-owned agent deleted")
	}
}

// Pause and resume (the emergency stop) stay possible on a Git-owned Agent, with the API's own field manager.
func TestGitOwnedAgentCanBePaused(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := gitEnv(t, gitea.URL)
	w := e.call("PATCH", "/api/agents/alice", "writer", `{"paused":true}`)
	if w.Code != 200 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	var got agent
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Paused || got.Source != "git" {
		t.Errorf("response %+v, want paused, source git", got)
	}
	if w := e.call("PATCH", "/api/agents/alice", "writer", `{"paused":false}`); w.Code != 200 {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if agentPatchOptions().FieldManager != "agentdev-api" {
		t.Errorf("field manager %q", agentPatchOptions().FieldManager)
	}
}

// An Agent the API made stays API-owned in git mode: Flux does not prune it, so the API may change and delete it.
func TestAPIOwnedAgentInGitMode(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := gitEnv(t, gitea.URL)
	if w := e.call("POST", "/api/agents", "admin", `{"name":"bob","repo":"owner/x"}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := e.call("PATCH", "/api/agents/bob", "writer", `{"maxConcurrent":3}`); w.Code != 200 {
		t.Errorf("patch API-owned: %d %s", w.Code, w.Body)
	}
	list := e.call("GET", "/api/agents", "group", "")
	var resp struct{ Agents []agent }
	_ = json.Unmarshal(list.Body.Bytes(), &resp)
	src := map[string]string{}
	for _, a := range resp.Agents {
		src[a.Name] = a.Source
	}
	if src["alice"] != "git" || src["bob"] != "api" {
		t.Errorf("sources %v, want alice git, bob api", src)
	}
	if w := e.call("DELETE", "/api/agents/bob", "admin", ""); w.Code != 204 {
		t.Errorf("delete API-owned: %d %s", w.Code, w.Body)
	}
}

// api mode (the default) keeps the old behaviour: every Agent may be changed.
func TestAPIModeAllowsEdits(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	if w := e.call("PATCH", "/api/agents/alice", "writer", `{"maxConcurrent":4}`); w.Code != 200 {
		t.Errorf("api mode patch: %d %s", w.Code, w.Body)
	}
	u := &unstructured.Unstructured{}
	u.SetName("x")
	if sourceOf(agentsSourceAPI, u) != "api" || sourceOf(agentsSourceGit, u) != "git" {
		t.Error("sourceOf")
	}
}
