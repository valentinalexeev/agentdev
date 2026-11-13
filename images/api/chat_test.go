package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// fakeSessionServer behaves like chat-session-server for the API: token check, an event log, messages,
// permission answers, presence and end.
type fakeSessionServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	token    string
	events   []map[string]any
	pending  map[string]bool
	viewers  []string
	ended    bool
	messages []string
}

func newFakeSessionServer(t *testing.T) *fakeSessionServer {
	f := &fakeSessionServer{pending: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSessionServer) add(typ, author string, data any) {
	b, _ := json.Marshal(data)
	ev := map[string]any{"seq": len(f.events) + 1, "time": time.Now().UTC(), "type": typ, "data": json.RawMessage(b)}
	if author != "" {
		ev["author"] = author
	}
	f.events = append(f.events, ev)
}

func (f *fakeSessionServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		after := 0
		_, _ = fmtSscan(r.URL.Query().Get("after"), &after)
		out := []map[string]any{}
		for _, e := range f.events {
			if e["seq"].(int) > after {
				out = append(out, e)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": out, "state": map[string]any{"viewers": f.viewers}})
	case r.URL.Path == "/messages":
		f.messages = append(f.messages, body["text"].(string))
		f.add("user", body["author"].(string), map[string]any{"text": body["text"]})
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case strings.HasPrefix(r.URL.Path, "/permissions/"):
		id := strings.TrimPrefix(r.URL.Path, "/permissions/")
		if !f.pending[id] {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no pending permission request with this id"})
			return
		}
		delete(f.pending, id)
		f.add("permission-answer", body["author"].(string), map[string]any{"id": id, "behavior": body["behavior"]})
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case r.URL.Path == "/presence":
		f.viewers = nil
		for _, v := range body["viewers"].([]any) {
			f.viewers = append(f.viewers, v.(string))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case r.URL.Path == "/end":
		f.ended = true
		f.add("end", body["author"].(string), map[string]any{"reason": "ended by a person"})
		_ = json.NewEncoder(w).Encode(map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

type fakeChatCluster struct {
	mu      sync.Mutex
	server  *fakeSessionServer
	started []chatSession
	stopped []string
	gone    bool
}

func (c *fakeChatCluster) StartSession(_ context.Context, s chatSession, sha string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, s)
	return nil
}

func (c *fakeChatCluster) SessionAddr(context.Context, string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return "", errChatGone
	}
	return strings.TrimPrefix(c.server.srv.URL, "http://"), nil
}

func (c *fakeChatCluster) StopSession(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = append(c.stopped, id)
	c.gone = true
	return nil
}

type fakeIssues struct {
	mu    sync.Mutex
	calls []map[string]string
}

func (f *fakeIssues) CreateIssue(_ context.Context, forge, repo, as, title, body string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, map[string]string{"repo": repo, "as": as, "title": title, "body": body})
	return 7, "http://forge/owner/x/issues/7", nil
}

type chatEnv struct {
	*tenv
	c       *chatAPI
	server  *fakeSessionServer
	cluster *fakeChatCluster
	issues  *fakeIssues
	store   *memChat
}

func newChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	g := fakeGitea(t)
	t.Cleanup(g.Close)
	e := newEnv(t, g.URL)
	srv := newFakeSessionServer(t)
	cl := &fakeChatCluster{server: srv}
	is := &fakeIssues{}
	st := newMemChat()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := newChatAPI(ctx, e.wr, e.k, st, cl, is, chatConfig{TokenKey: []byte(strings.Repeat("k", 32)), Retention: 30 * 24 * time.Hour})
	c.nowFn = func() time.Time { return time.Now().UTC() }
	old := chatSvc
	chatSvc = c
	t.Cleanup(func() { chatSvc = old })
	e.h = routes(idVerify, e.k, nil, e.wr)
	return &chatEnv{tenv: e, c: c, server: srv, cluster: cl, issues: is, store: st}
}

// start starts a session as the writer and points the fake server's token at it.
func (e *chatEnv) start(t *testing.T) string {
	t.Helper()
	w := e.call("POST", "/api/sessions", "writer", `{"agent":"alice"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	e.server.mu.Lock()
	e.server.token = e.c.token(v.ID)
	e.server.mu.Unlock()
	return v.ID
}

func (e *chatEnv) events(t *testing.T, sid string) []chatEvent {
	t.Helper()
	evs, _ := e.store.Events(context.Background(), sid, 0, 1000)
	return evs
}

func TestChatStartNeedsWrite(t *testing.T) {
	e := newChatEnv(t)
	for tok, want := range map[string]int{"reader": 403, "nobody": 403, "group": 403, "": 401} {
		if w := e.call("POST", "/api/sessions", tok, `{"agent":"alice"}`); w.Code != want {
			t.Errorf("%q: got %d, want %d (%s)", tok, w.Code, want, w.Body)
		}
	}
	if w := e.call("POST", "/api/sessions", "writer", `{"agent":"nope"}`); w.Code != 404 {
		t.Errorf("unknown agent: %d", w.Code)
	}
	if w := e.call("POST", "/api/sessions", "writer", `{"agent":"alice","image":"x"}`); w.Code != 400 {
		t.Errorf("unknown field: %d", w.Code)
	}
	sid := e.start(t)
	if len(e.cluster.started) != 1 || e.cluster.started[0].Repo != "owner/x" || e.cluster.started[0].StartedBy != "wendy" {
		t.Fatalf("job: %+v", e.cluster.started)
	}
	if a := e.lastAudit(t); a.Action != "chat-start" || a.Result != "ok" || a.Who.Username != "wendy" {
		t.Errorf("audit: %+v", a)
	}
	if !chatIDRe.MatchString(sid) {
		t.Errorf("id %q", sid)
	}
}

func TestChatReaderCanViewNotSend(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	// the reader sees the session and its transcript
	w := e.call("GET", "/api/sessions", "reader", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), sid) || !strings.Contains(w.Body.String(), `"access":"read"`) {
		t.Fatalf("reader list: %d %s", w.Code, w.Body)
	}
	if w := e.call("GET", "/api/sessions/"+sid, "reader", ""); w.Code != 200 {
		t.Fatalf("reader get: %d", w.Code)
	}
	// but cannot send, answer, end or create
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/sessions/" + sid + "/messages", `{"text":"hi"}`},
		{"POST", "/api/sessions/" + sid + "/permissions/p1", `{"behavior":"allow"}`},
		{"POST", "/api/sessions/" + sid + "/drafts/d1/create", `{}`},
		{"DELETE", "/api/sessions/" + sid, ``},
	} {
		if w := e.call(c.m, c.p, "reader", c.b); w.Code != 403 {
			t.Errorf("%s %s: %d %s", c.m, c.p, w.Code, w.Body)
		}
	}
	if a := e.lastAudit(t); a.Result != "denied" || a.Who.Username != "rita" {
		t.Errorf("audit of the refusal: %+v", a)
	}
	if len(e.server.messages) != 0 {
		t.Errorf("a message reached the session: %v", e.server.messages)
	}
	// the writer can
	if w := e.call("POST", "/api/sessions/"+sid+"/messages", "writer", `{"text":"hello"}`); w.Code != 202 {
		t.Fatalf("writer message: %d %s", w.Code, w.Body)
	}
	if got := e.server.messages; len(got) != 1 || got[0] != "hello" {
		t.Errorf("messages: %v", got)
	}
	evs := e.events(t, sid)
	if last := evs[len(evs)-1]; last.Type != "user" || last.Author != "wendy" {
		t.Errorf("event log: %+v", last)
	}
}

func TestChatNonMemberSeesNothing(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	w := e.call("GET", "/api/sessions", "nobody", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), sid) {
		t.Fatalf("non-member list: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/sessions/" + sid, "/api/sessions/" + sid + "/events"} {
		if w := e.call("GET", p, "nobody", ""); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/messages", "nobody", `{"text":"x"}`); w.Code != 404 {
		t.Errorf("message: %d", w.Code)
	}
	// admins read every session (not send)
	if w := e.call("GET", "/api/sessions/"+sid, "group", ""); w.Code != 200 {
		t.Errorf("admin group read: %d", w.Code)
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/messages", "group", `{"text":"x"}`); w.Code != 403 {
		t.Errorf("admin group send: %d", w.Code)
	}
}

func TestChatNoIssueWithoutCreate(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	e.server.mu.Lock()
	e.server.add("assistant", "", map[string]any{"text": "Here is a draft"})
	e.server.add("draft", "", map[string]any{"id": "d1", "title": "Add X", "what": "w", "acceptance": "a"})
	e.server.mu.Unlock()
	e.c.pumpOnce(sid)
	if len(e.issues.calls) != 0 {
		t.Fatalf("an issue was created without the create call: %v", e.issues.calls)
	}
	// the model cannot fake an issue-created event (unknown type from the server is dropped)
	e.server.mu.Lock()
	e.server.add("issue-created", "", map[string]any{"draft": "d1"})
	e.server.mu.Unlock()
	e.c.pumpOnce(sid)
	for _, ev := range e.events(t, sid) {
		if ev.Type == "issue-created" {
			t.Fatalf("session server wrote issue-created: %+v", ev)
		}
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/drafts/d9/create", "writer", `{}`); w.Code != 404 {
		t.Errorf("unknown draft: %d", w.Code)
	}
	w := e.call("POST", "/api/sessions/"+sid+"/drafts/d1/create", "writer", `{"title":"Add X, edited"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if len(e.issues.calls) != 1 || e.issues.calls[0]["as"] != "wendy" || e.issues.calls[0]["title"] != "Add X, edited" ||
		!strings.Contains(e.issues.calls[0]["body"], "## Acceptance criterion") {
		t.Fatalf("issue: %v", e.issues.calls)
	}
	if a := e.lastAudit(t); a.Action != "chat-create-issue" || a.Result != "ok" {
		t.Errorf("audit: %+v", a)
	}
	// a second click does not create a second issue
	if w := e.call("POST", "/api/sessions/"+sid+"/drafts/d1/create", "writer", `{}`); w.Code != 409 || len(e.issues.calls) != 1 {
		t.Errorf("second create: %d, calls %d", w.Code, len(e.issues.calls))
	}
}

func TestChatPermissionAnswer(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	e.server.mu.Lock()
	e.server.pending["p1"] = true
	e.server.add("permission", "", map[string]any{"id": "p1", "tool": "Bash", "input": "rm -rf x"})
	e.server.mu.Unlock()
	if w := e.call("POST", "/api/sessions/"+sid+"/permissions/p1", "writer", `{"behavior":"maybe"}`); w.Code != 400 {
		t.Errorf("bad behavior: %d", w.Code)
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/permissions/p1", "writer", `{"behavior":"allow"}`); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/permissions/p1", "admin", `{"behavior":"deny"}`); w.Code != 404 {
		t.Errorf("second answer: %d %s", w.Code, w.Body)
	}
	var found bool
	for _, ev := range e.events(t, sid) {
		if ev.Type == "permission-answer" && ev.Author == "wendy" && strings.Contains(string(ev.Data), `"allow"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("answer with name not in the transcript: %+v", e.events(t, sid))
	}
}

func TestChatEndAndReadOnly(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	if w := e.call("DELETE", "/api/sessions/"+sid, "writer", ""); w.Code != 200 {
		t.Fatalf("end: %d %s", w.Code, w.Body)
	}
	if len(e.cluster.stopped) != 1 {
		t.Errorf("job not stopped")
	}
	s, _ := e.store.GetSession(context.Background(), sid)
	if s.EndedAt == nil {
		t.Fatalf("not ended")
	}
	if w := e.call("POST", "/api/sessions/"+sid+"/messages", "writer", `{"text":"x"}`); w.Code != 409 {
		t.Errorf("message after end: %d", w.Code)
	}
	// the transcript stays readable
	if w := e.call("GET", "/api/sessions/"+sid+"/events", "reader", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "event: chat-event") ||
		!strings.Contains(w.Body.String(), `"type":"end"`) || !strings.Contains(w.Body.String(), "event: bye") {
		t.Errorf("transcript stream: %d %s", w.Code, w.Body)
	}
}

func TestChatIdleStop(t *testing.T) {
	// the session server ends the session on idle (tested in chat-session-server.test.js); the API sees the end
	// event or the gone pod and closes the session
	e := newChatEnv(t)
	sid := e.start(t)
	e.server.mu.Lock()
	e.server.add("end", "", map[string]any{"reason": "nobody here for 30 min"})
	e.server.mu.Unlock()
	e.c.pumpOnce(sid)
	s, _ := e.store.GetSession(context.Background(), sid)
	if s.EndedAt == nil || s.EndedReason != "nobody here for 30 min" {
		t.Fatalf("not ended on idle: %+v", s)
	}
	// a pod that is gone ends the session too
	sid2 := e.start(t)
	e.cluster.mu.Lock()
	e.cluster.gone = true
	e.cluster.mu.Unlock()
	gone, _ := e.c.pumpStep(context.Background(), sid2)
	if !gone {
		t.Fatal("gone pod not seen")
	}
}

func TestChatPresenceDrivesIdle(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	e.c.join(sid, "wendy")
	e.c.pumpOnce(sid)
	if got := e.server.viewers; len(got) != 1 || got[0] != "wendy" {
		t.Fatalf("presence: %v", got)
	}
	e.c.leave(sid, "wendy")
	base := time.Now().UTC()
	e.c.nowFn = func() time.Time { return base.Add(time.Minute) }
	e.c.pumpOnce(sid)
	if got := e.server.viewers; len(got) != 0 {
		t.Fatalf("viewer kept after leaving: %v", got)
	}
}

func TestChatEventLogAppendOnly(t *testing.T) {
	e := newChatEnv(t)
	sid := e.start(t)
	e.server.mu.Lock()
	e.server.add("assistant", "", map[string]any{"text": "one"})
	e.server.add("bogus", "", map[string]any{})
	e.server.mu.Unlock()
	e.c.pumpOnce(sid)
	e.c.pumpOnce(sid) // a second copy does not duplicate
	var types []string
	for _, ev := range e.events(t, sid) {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "start,assistant" {
		t.Fatalf("log: %v", types)
	}
	s, _ := e.store.GetSession(context.Background(), sid)
	if s.LastSeq != 2 {
		t.Errorf("last seq %d", s.LastSeq)
	}
}

func TestChatTokenPerSession(t *testing.T) {
	c := &chatAPI{cfg: chatConfig{TokenKey: []byte(strings.Repeat("k", 32))}}
	if c.token("chat-a-00000001") == c.token("chat-a-00000002") || len(tokenSHA(c.token("x"))) != 64 {
		t.Fatal("tokens must differ per session")
	}
}

func TestChatJobShape(t *testing.T) {
	k := &kubeChat{c: fake.NewSimpleClientset(), cfg: chatJobConfig{Namespace: "coding-agents", WorkerImage: "w:1", PullPolicy: "IfNotPresent",
		ForgeURL: "http://gitea", ClaudeProxyURL: "http://proxy", EgressProxy: "http://egress", IdleSeconds: 1800, MaxSeconds: 28800}}
	j := k.job(chatSession{ID: "chat-alice-0000abcd", Agent: "alice", Repo: "owner/x", StartedBy: "wendy"}, strings.Repeat("a", 64))
	p := j.Spec.Template.Spec
	if *p.RuntimeClassName != "gvisor" || *p.AutomountServiceAccountToken || j.Spec.Template.Labels["app"] != "agent-chat" {
		t.Fatalf("pod: %+v", p)
	}
	for _, e := range p.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "agent-alice-proxy" {
			t.Errorf("chat container mounts secret %s", e.ValueFrom.SecretKeyRef.Name)
		}
		if strings.Contains(e.Name, "FORGE_TOKEN") {
			t.Errorf("chat container has a forge token")
		}
	}
	if err := k.StartSession(context.Background(), chatSession{ID: "chat-alice-0000abcd", Agent: "alice", Repo: "owner/x"}, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := k.StopSession(context.Background(), "chat-alice-0000abcd"); err != nil {
		t.Fatal(err)
	}
	if err := k.StopSession(context.Background(), "chat-alice-0000abcd"); err != errChatGone {
		t.Fatalf("second stop: %v", err)
	}
}

func TestChatPrune(t *testing.T) {
	m := newMemChat()
	old := time.Now().Add(-40 * 24 * time.Hour)
	ended := old.Add(time.Hour)
	_ = m.CreateSession(context.Background(), chatSession{ID: "old", StartedAt: old, EndedAt: &ended})
	_ = m.CreateSession(context.Background(), chatSession{ID: "new", StartedAt: time.Now()})
	_ = m.AppendEvents(context.Background(), "old", []chatEvent{{Seq: 1, Type: "user"}}, 1)
	_ = m.AppendEvents(context.Background(), "new", []chatEvent{{Seq: 1, Type: "user"}}, 1)
	now = func() time.Time { return time.Now().UTC() }
	if n, _ := m.PruneChat(context.Background()); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if evs, _ := m.Events(context.Background(), "old", 0, 10); len(evs) != 0 {
		t.Errorf("events of the pruned session remain")
	}
	if _, err := m.GetSession(context.Background(), "new"); err != nil {
		t.Errorf("new session pruned")
	}
}
