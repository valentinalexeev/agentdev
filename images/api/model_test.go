package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func withModel(u *unstructured.Unstructured, model, effort string) *unstructured.Unstructured {
	if model != "" {
		_ = unstructured.SetNestedField(u.Object, model, "spec", "model")
	}
	if effort != "" {
		_ = unstructured.SetNestedField(u.Object, effort, "spec", "effort")
	}
	return u
}

func modelEntries(t *testing.T, m *memAudit, agent string) []auditEntry {
	t.Helper()
	recs, _ := m.ListAudit(context.Background(), auditFilter{Agent: agent, Limit: 100})
	var out []auditEntry
	for i := len(recs) - 1; i >= 0; i-- {
		var e auditEntry
		if err := json.Unmarshal(recs[i].Entry, &e); err != nil {
			t.Fatal(err)
		}
		if e.Action == modelAction {
			out = append(out, e)
		}
	}
	return out
}

// TestModelAudit (#570): a change of spec.model or spec.effort makes exactly one "model" entry with the old and new
// values, like spec.permission (#505); a relist, a restart of the API or a change of another field makes none.
func TestModelAudit(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	var out bytes.Buffer
	old := auditOut
	auditOut = &out
	t.Cleanup(func() { auditOut = old })
	ctx := context.Background()
	a := &modelAudit{}

	a.observe(ctx, agentObj("alice", "owner/x", false)) // neither field: nothing to record
	if n := len(modelEntries(t, m, "alice")); n != 0 {
		t.Fatalf("default made %d entries", n)
	}
	a.observe(ctx, withModel(agentObj("alice", "owner/x", false), "sonnet", ""))
	a.observe(ctx, withModel(agentObj("alice", "owner/x", true), "sonnet", "")) // paused only
	es := modelEntries(t, m, "alice")
	if len(es) != 1 || es[0].Before != nil || es[0].Reason != firstSeenReason || *es[0].After.Claude != (agentClaude{Model: "sonnet"}) {
		t.Fatalf("%+v", es)
	}
	// a restarted API (empty cache) reads the last entry: no duplicate; a change while it was down is recorded once
	(&modelAudit{}).observe(ctx, withModel(agentObj("alice", "owner/x", false), "sonnet", ""))
	(&modelAudit{}).observe(ctx, withModel(agentObj("alice", "owner/x", false), "opus", "high"))
	(&modelAudit{}).observe(ctx, withModel(agentObj("alice", "owner/x", false), "opus", "high"))
	es = modelEntries(t, m, "alice")
	if len(es) != 2 || *es[1].Before.Claude != (agentClaude{Model: "sonnet"}) || *es[1].After.Claude != (agentClaude{Model: "opus", Effort: "high"}) {
		t.Fatalf("%+v", es)
	}
	// back to the default, and the permission entries are not touched by it
	a.observe(ctx, agentObj("alice", "owner/x", false))
	es = modelEntries(t, m, "alice")
	if len(es) != 3 || *es[2].After.Claude != (agentClaude{}) || len(permEntries(t, m, "alice")) != 0 {
		t.Fatalf("%+v", es)
	}
	if n := strings.Count(out.String(), `AGENTDEV_AUDIT {`); n != 3 || !strings.Contains(out.String(), `"claude":{"model":"opus","effort":"high"}`) {
		t.Fatalf("stdout lines: %d %s", n, out.String())
	}
}

// #570: GET /api/agents and the agent detail show spec.model and spec.effort.
func TestAgentListModel(t *testing.T) {
	e := newChatEnv(t)
	if _, err := e.k.Patch(context.Background(), "alice", []byte(`{"spec":{"model":"sonnet","effort":"high"}}`)); err != nil {
		t.Fatal(err)
	}
	w := e.call("GET", "/api/agents", "writer", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"sonnet","effort":"high"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// newModelChatEnv: alice runs sonnet at effort high; the instance allows opus and haiku and the effort low in chat.
func newModelChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	e := newChatEnv(t)
	e.c.cfg.Models, e.c.cfg.Efforts = []string{"opus", "haiku"}, []string{"low"}
	if _, err := e.k.Patch(context.Background(), "alice", []byte(`{"spec":{"model":"sonnet","effort":"high"}}`)); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestChatStartModel (#570): a session starts with the agent's model and effort, or with a value of the allow-list.
func TestChatStartModel(t *testing.T) {
	e := newModelChatEnv(t)
	for _, body := range []string{`{"agent":"alice","model":"gpt-5"}`, `{"agent":"alice","model":"fable"}`,
		`{"agent":"alice","effort":"max"}`, `{"agent":"alice","model":""}`, `{"agent":"alice","model":5}`} {
		if w := e.call("POST", "/api/sessions", "writer", body); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if a := e.lastAudit(t); a.Action != "chat-start" || a.Result != "invalid" {
		t.Errorf("audit of the refusal: %+v", a)
	}
	if len(e.cluster.started) != 0 {
		t.Fatalf("a refused start made a Job: %+v", e.cluster.started)
	}
	e.start(t)
	if w := e.call("POST", "/api/sessions", "writer", `{"agent":"alice","model":"opus","effort":"low"}`); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.call("POST", "/api/sessions", "writer", `{"agent":"alice","model":"sonnet"}`); w.Code != 201 {
		t.Fatalf("own model: %d %s", w.Code, w.Body)
	}
	got := [][2]string{}
	for _, s := range e.cluster.started {
		got = append(got, [2]string{s.Model, s.Effort})
	}
	if want := [][2]string{{"sonnet", "high"}, {"opus", "low"}, {"sonnet", "high"}}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("started with %v, want %v", got, want)
	}
	if a := e.lastAudit(t); a.Action != "chat-start" || !strings.Contains(jsonText(a.Request), `"model":"sonnet"`) {
		t.Errorf("audit: %+v", a.Request)
	}
	// the allow-list and the multipliers for the cost hint
	old := modelPrices
	modelPrices = func(context.Context) map[string]float64 { return map[string]float64{"claude-opus": 1.67, "claude-sonnet": 1} }
	t.Cleanup(func() { modelPrices = old })
	w := e.call("GET", "/api/sessions", "reader", "")
	if !strings.Contains(w.Body.String(), `"options":{"models":["opus","haiku"],"efforts":["low"],"multipliers":{"claude-opus":1.67,"claude-sonnet":1}}`) {
		t.Errorf("options: %s", w.Body)
	}
}

// TestChatSettings (#570): a writer changes the model or effort of a running session within the allow-list; the change
// reaches the session server, is an audit entry (chat-settings) and a "settings" event of the transcript.
func TestChatSettings(t *testing.T) {
	e := newModelChatEnv(t)
	sid := e.start(t)
	path := "/api/sessions/" + sid + "/settings"
	if w := e.call("POST", path, "reader", `{"model":"opus"}`); w.Code != 403 {
		t.Errorf("reader: %d", w.Code)
	}
	for _, body := range []string{`{"model":"gpt-5"}`, `{"model":"fable"}`, `{"effort":"medium"}`, `{"model":""}`, `{}`, `{"model":"opus","x":1}`} {
		if w := e.call("POST", path, "writer", body); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if a := e.lastAudit(t); a.Action != "chat-settings" || a.Result != "invalid" {
		t.Errorf("audit of the refusal: %+v", a)
	}
	e.server.mu.Lock()
	n := len(e.server.settings)
	e.server.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused change reached the session: %d", n)
	}
	if w := e.call("POST", path, "writer", `{"model":"opus","effort":"low"}`); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.call("POST", path, "writer", `{"effort":"high"}`); w.Code != 202 { // back to the agent's own
		t.Fatalf("own effort: %d %s", w.Code, w.Body)
	}
	e.server.mu.Lock()
	sent := jsonText(e.server.settings)
	e.server.mu.Unlock()
	if sent != `[{"author":"wendy","effort":"low","model":"opus"},{"author":"wendy","effort":"high"}]` {
		t.Errorf("sent %s", sent)
	}
	if a := e.lastAudit(t); a.Action != "chat-settings" || a.Result != "ok" || jsonText(a.Request) != `{"repo":"owner/x","request":{"effort":"high"},"session":"`+sid+`"}` {
		t.Errorf("audit: %+v", a)
	}
	var kinds []string
	for _, ev := range e.events(t, sid) {
		if ev.Type == "settings" {
			kinds = append(kinds, string(ev.Data))
		}
	}
	if len(kinds) != 3 || !strings.Contains(kinds[0], `"value":"opus"`) {
		t.Errorf("settings events %v", kinds)
	}
	// an ended session is read-only
	e.call("DELETE", "/api/sessions/"+sid, "writer", "")
	if w := e.call("POST", path, "writer", `{"model":"opus"}`); w.Code != 409 {
		t.Errorf("ended: %d", w.Code)
	}
}

// #570: the start values reach the chat container as AGENT_MODEL and AGENT_EFFORT, and the admission policy takes it.
func TestChatPodModelEnv(t *testing.T) {
	k := &kubeChat{cfg: chatJobConfig{Namespace: "coding-agents", WorkerImage: testWorkerImage, IdleSeconds: 1800, MaxSeconds: 28800}}
	j, err := k.jobFrom(agentFixture(t, "gitea"), chatSession{ID: "chat-alice-0000abcd", Agent: "alice", Repo: "owner/repo",
		StartedBy: "wendy", Model: "opus", Effort: "low"}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	p := j.Spec.Template.Spec
	env := envMap(containerNamed(p, "chat"))
	if env["AGENT_MODEL"].Value != "opus" || env["AGENT_EFFORT"].Value != "low" {
		t.Errorf("chat env %v %v", env["AGENT_MODEL"], env["AGENT_EFFORT"])
	}
	for _, c := range containerNamed(p, "forge").Env {
		if c.Name == "AGENT_MODEL" || c.Name == "AGENT_EFFORT" {
			t.Errorf("forge container has %s", c.Name)
		}
	}
	if f := admit(t, chatPolicy(t), j); len(f) != 0 {
		t.Errorf("refused: %v", f)
	}
	// no start values: nothing added (the agent's own env stays as the operator rendered it)
	env = envMap(containerNamed(chatJobFor(t, "gitea").Spec.Template.Spec, "chat"))
	if _, ok := env["AGENT_MODEL"]; ok {
		t.Errorf("AGENT_MODEL without a choice: %v", env["AGENT_MODEL"])
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

