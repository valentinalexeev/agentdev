package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// labelAsked calls the internal listener as the forge sidecar at addr would.
func (e *chatEnv) labelAsked(sid, from, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("POST /internal/chat/{id}/label-asked", e.c.labelAsked())
	r := httptest.NewRequest("POST", "/internal/chat/"+sid+"/label-asked", strings.NewReader(body))
	r.RemoteAddr = from
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// (e *chatEnv) permission adds a pending permission request of the session server.
func (e *chatEnv) permission(sid, pid, tool, input string) {
	e.server.mu.Lock()
	e.server.pending[pid] = true
	e.server.add("permission", "", map[string]any{"id": pid, "tool": tool, "input": input})
	e.server.mu.Unlock()
	e.c.pumpOnce(sid)
}

// #571: the sidecar of a chat pod gets a person only for a label call that person allowed in the console, once, from
// the session's own pod; each answer is audited with the person (or refused without one).
func TestChatLabelAsked(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	pod := "127.0.0.1:40000" // the fake cluster reports the session at 127.0.0.1
	agentLabel := `{"repo":"owner/x","issue":570,"labels":["agent:alice"]}`
	cmd := func(c string) string { return `{"command":` + jsonString(c) + `,"description":"label the issue"}` }

	// nobody allowed anything yet
	if w := e.labelAsked(sid, pod, agentLabel); w.Code != http.StatusForbidden {
		t.Fatalf("before an allow: %d %s", w.Code, w.Body)
	}
	if a := e.lastAudit(t); a.Action != "chat-label" || a.Result != "denied" || a.Who.ForgeUser != "" {
		t.Errorf("refusal audit: %+v", a)
	}

	e.permission(sid, "p1", "Bash", cmd(`agentdev-forge api POST /repos/owner/x/issues/570/labels '{"labels":["agent:alice"]}'`))
	if w := e.call("POST", "/api/sessions/"+sid+"/permissions/p1", "writer", `{"behavior":"allow"}`); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if w := e.labelAsked(sid, "127.0.0.2:40000", agentLabel); w.Code != http.StatusForbidden {
		t.Errorf("another pod: %d", w.Code)
	}
	for name, b := range map[string]string{
		"other issue":          `{"repo":"owner/x","issue":57,"labels":["agent:alice"]}`,
		"other label":          `{"repo":"owner/x","issue":570,"labels":["agent:bob"]}`,
		"one more label":       `{"repo":"owner/x","issue":570,"labels":["agent:alice","bug"]}`,
		"other repo":           `{"repo":"owner/y","issue":570,"labels":["agent:alice"]}`,
		"unknown key (400)":    `{"repo":"owner/x","issue":570,"labels":["agent:alice"],"person":"wendy"}`,
		"no labels (400)":      `{"repo":"owner/x","issue":570,"labels":[]}`,
		"bad repo (400)":       `{"repo":"../x","issue":570,"labels":["agent:alice"]}`,
		"issue not set (400)":  `{"repo":"owner/x","labels":["agent:alice"]}`,
		"label not set (400)":  `{"repo":"owner/x","issue":570,"labels":[""]}`,
		"not JSON (400)":       `repo=owner/x`,
		"labels not a list":    `{"repo":"owner/x","issue":570,"labels":"agent:alice"}`,
		"issue as text (400)":  `{"repo":"owner/x","issue":"570","labels":["agent:alice"]}`,
		"negative issue (400)": `{"repo":"owner/x","issue":-570,"labels":["agent:alice"]}`,
	} {
		if w := e.labelAsked(sid, pod, b); w.Code/100 != 4 {
			t.Errorf("%s: want 4xx, got %d", name, w.Code)
		}
	}
	w := e.labelAsked(sid, pod, agentLabel)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"person":"wendy"`) {
		t.Fatalf("allowed call: %d %s", w.Code, w.Body)
	}
	a := e.lastAudit(t)
	if a.Action != "chat-label" || a.Result != "ok" || a.Who.ForgeUser != "wendy" || a.Agent != "alice" {
		t.Errorf("audit: %+v", a)
	}
	if rq, _ := a.Request.(map[string]any); rq["permission"] != "p1" || rq["session"] != sid {
		t.Errorf("audit request: %+v", a.Request)
	}
	// one allow is one label write
	if w := e.labelAsked(sid, pod, agentLabel); w.Code != http.StatusForbidden {
		t.Errorf("second write on one allow: %d", w.Code)
	}

	// the description names the call, the command does not: refused
	e.permission(sid, "p2", "Bash", `{"command":"./label.sh","description":"agentdev-forge api POST /repos/owner/x/issues/570/labels \"agent:alice\""}`)
	// a denied request never counts
	e.permission(sid, "p3", "Bash", cmd(`agentdev-forge api POST /repos/owner/x/issues/570/labels '{"labels":["agent:alice"]}'`))
	// a tool that is not Bash
	e.permission(sid, "p4", "Write", `{"file_path":"/repos/owner/x/issues/570/labels","content":"\"agent:alice\""}`)
	_ = e.call("POST", "/api/sessions/"+sid+"/permissions/p2", "writer", `{"behavior":"allow"}`)
	_ = e.call("POST", "/api/sessions/"+sid+"/permissions/p3", "writer", `{"behavior":"deny"}`)
	_ = e.call("POST", "/api/sessions/"+sid+"/permissions/p4", "writer", `{"behavior":"allow"}`)
	if w := e.labelAsked(sid, pod, agentLabel); w.Code != http.StatusForbidden {
		t.Errorf("description, denied or other tool: %d", w.Code)
	}

	// a label in a quoted shell argument (\"name\"), plain labels; then the allow expires
	e.permission(sid, "p5", "Bash", cmd(`agentdev-forge api POST /repos/owner/x/issues/12/labels "{\"labels\":[\"bug\",\"needs triage\"]}"`))
	_ = e.call("POST", "/api/sessions/"+sid+"/permissions/p5", "writer", `{"behavior":"allow"}`)
	e.c.nowFn = func() time.Time { return time.Now().UTC().Add(chatAllowTTL) }
	if w := e.labelAsked(sid, pod, `{"repo":"owner/x","issue":12,"labels":["bug","needs triage"]}`); w.Code != http.StatusForbidden {
		t.Errorf("expired allow: %d", w.Code)
	}
	e.c.nowFn = func() time.Time { return time.Now().UTC() }
	if w := e.labelAsked(sid, pod, `{"repo":"owner/x","issue":12,"labels":["bug","needs triage"]}`); w.Code != 200 {
		t.Errorf("escaped labels: %d %s", w.Code, w.Body)
	}

	// the session pod is gone
	e.cluster.mu.Lock()
	e.cluster.gone = true
	e.cluster.mu.Unlock()
	e.permission(sid, "p6", "Bash", cmd(`agentdev-forge api POST /repos/owner/x/issues/570/labels '{"labels":["agent:alice"]}'`))
	if w := e.labelAsked(sid, pod, agentLabel); w.Code != http.StatusForbidden {
		t.Errorf("pod gone: %d", w.Code)
	}
	if w := e.labelAsked("chat-nobody-0000000a", pod, agentLabel); w.Code != http.StatusNotFound {
		t.Errorf("unknown session: %d", w.Code)
	}
}

func TestChatLabelCommand(t *testing.T) {
	for _, c := range []struct {
		cmd    string
		issue  int64
		labels []string
		want   bool
	}{
		{`x POST /repos/o/r/issues/570/labels '{"labels":["a"]}'`, 570, []string{"a"}, true},
		{`x POST /repos/o/r/issues/570/labels '{"labels":["a"]}'`, 57, []string{"a"}, false},
		{`x POST /repos/o/r/issues/57/labels '{"labels":["a"]}'`, 570, []string{"a"}, false},
		{`x POST /repos/o/r/issues/570/labels '{"labels":["debug"]}'`, 570, []string{"bug"}, false},
		{`x POST /repos/o/r/issues/570/labels "{\"labels\":[\"bug\"]}"`, 570, []string{"bug"}, true},
		{`x POST /repos/o/r2/issues/570/labels '{"labels":["a"]}'`, 570, []string{"a"}, false},
	} {
		if got := chatLabelCommand(c.cmd, "o/r", c.issue, c.labels); got != c.want {
			t.Errorf("%s %d %v: got %v", c.cmd, c.issue, c.labels, got)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
