package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunEvents(t *testing.T) {
	var got []string
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	var log bytes.Buffer
	old := eventOut
	eventOut = &log
	t.Cleanup(func() { eventOut = old })
	s := newServer(&Config{ForgeUser: "alice", AgentName: "alice", Token: "forge-token-abcdef", Repos: []string{"o/r"}, APIURL: api.URL})
	h := s.routes()

	long := strings.Repeat("y", 9000)
	body := `{"job":"issue-3","events":[{"kind":"text","time":"2026-11-16T10:00:00Z","text":"<img src=x onerror=alert(1)> ghp_` +
		strings.Repeat("a", 30) + ` forge-token-abcdef"},{"kind":"tool_result","time":"x","ok":false,"size":9000,"text":"` + long + `"}]}`
	if rec := call(t, h, "POST", "/run-events", body); rec.Code != 204 {
		t.Fatalf("valid batch: %d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 || paths[0] != "/internal/run-events/alice" {
		t.Fatalf("forwarded: %v %v", paths, got)
	}
	var fw runEventBatch
	if err := json.Unmarshal([]byte(got[0]), &fw); err != nil {
		t.Fatal(err)
	}
	if fw.Repo != "o/r" || fw.Job != "issue-3" || len(fw.Events) != 2 {
		t.Fatalf("batch: %+v", fw)
	}
	if e := fw.Events[0].Text; strings.Contains(e, "ghp_aaaa") || strings.Contains(e, "forge-token-abcdef") || !strings.Contains(e, "<img") {
		t.Fatalf("redaction: %q", e)
	}
	if e := fw.Events[1]; len(e.Text) > runEventMaxText+20 || !strings.HasSuffix(e.Text, "[clipped]") || e.Time == "x" {
		t.Fatalf("clip/time: %d %q", len(e.Text), e.Time)
	}
	if strings.Contains(log.String(), "onerror") {
		t.Fatal("model text reached the container log")
	}

	bad := []string{
		`{"job":"../x","events":[{"kind":"text","time":"","text":"a"}]}`,
		`{"job":"issue-1","repo":"other/repo","events":[{"kind":"text","time":"","text":"a"}]}`,
		`{"job":"issue-1","events":[{"kind":"html","time":"","text":"a"}]}`,
		`{"job":"issue-1","events":[]}`,
		`{"job":"issue-1","events":[{"kind":"text","time":"","size":-1}]}`,
		`{"job":"issue-1","events":[{"kind":"text","time":"","extra":1}]}`,
		`not json`,
	}
	for _, b := range bad {
		if rec := call(t, h, "POST", "/run-events", b); rec.Code != 400 {
			t.Errorf("%s: %d", b, rec.Code)
		}
	}
	if len(got) != 1 {
		t.Fatalf("a refused batch was forwarded: %d", len(got))
	}
	// #577: a refused batch names its repo, size and first event hash, never the event text
	if l := log.String(); !strings.Contains(l, `run events refused: repo is not one of this agent's (repo other/repo, 1 events, first event sha256 `) {
		t.Fatalf("refused repo not recorded: %s", l)
	}
	log.Reset()
	call(t, h, "POST", "/run-events", `{"job":"issue-1","repo":"x\ny","events":[{"kind":"text","time":"","text":"secret words"}]}`)
	if l := log.String(); !strings.Contains(l, "repo not of the form owner/name, 1 events") || strings.Contains(l, "secret words") {
		t.Fatalf("unsafe repo or event text in the event: %s", l)
	}
	if rec := call(t, h, "POST", "/run-events", `{"job":"issue-1","events":[`+strings.Repeat(`{"kind":"text","time":"","text":"a"},`, 101)+`{"kind":"text","time":""}]}`); rec.Code != 400 {
		t.Errorf("too many events: %d", rec.Code)
	}
	big := `{"job":"issue-1","events":[{"kind":"text","time":"","text":"` + strings.Repeat("z", runEventsMaxBody) + `"}]}`
	if rec := call(t, h, "POST", "/run-events", big); rec.Code != 413 && rec.Code != 400 {
		t.Errorf("too large: %d", rec.Code)
	}
	// loopback only
	req := httptest.NewRequest("POST", "/run-events", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.5:4444"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: %d", rec.Code)
	}
}

func TestRunEventsAPIDown(t *testing.T) {
	s := newServer(&Config{ForgeUser: "alice", AgentName: "alice", Repos: []string{"o/r"}, APIURL: "http://127.0.0.1:9"})
	rec := call(t, s.routes(), "POST", "/run-events", `{"job":"pr-2","events":[{"kind":"result","time":"","turns":2}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("api down: %d", rec.Code)
	}
	s = newServer(&Config{ForgeUser: "alice", AgentName: "alice", Repos: []string{"o/r"}})
	if rec := call(t, s.routes(), "POST", "/run-events", `{"job":"pr-2","events":[{"kind":"result","time":""}]}`); rec.Code != 204 {
		t.Fatalf("no api: %d", rec.Code)
	}
}

func TestRunEventsRedactBearerAndKeyTail(t *testing.T) {
	s := newServer(&Config{ForgeUser: "alice", AgentName: "alice", Repos: []string{"o/r"}})
	got := s.redactText("curl -H \"X: Bearer abcdefghijklmnop123\" ok\n-----BEGIN RSA PRIVATE KEY-----\nMIIEsecret\nmore")
	if strings.Contains(got, "abcdefghijklmnop123") || !strings.Contains(got, "Bearer [redacted]") {
		t.Fatalf("bearer: %q", got)
	}
	if strings.Contains(got, "MIIEsecret") || strings.Contains(got, "more") {
		t.Fatalf("key tail: %q", got)
	}
	if got := s.redactText("Bearer short"); got != "Bearer short" {
		t.Fatalf("short word redacted: %q", got)
	}
}
