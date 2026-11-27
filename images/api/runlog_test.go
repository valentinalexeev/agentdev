package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakePodIPs map[string][]string

func (f fakePodIPs) AgentPodIPs(_ context.Context, a string) ([]string, error) { return f[a], nil }

func postRun(t *testing.T, h http.Handler, from, agent, body string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "/internal/run-events/"+agent, strings.NewReader(body))
	r.SetPathValue("agent", agent)
	r.RemoteAddr = from + ":5555"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func runLogEnv(t *testing.T) (http.Handler, http.Handler, *memJobEvents) {
	t.Helper()
	h, wr := detailEnv(t)
	st := &memJobEvents{}
	wr.runlog = st
	rl := &runLog{store: st, agents: wr.store.(agentLister), pods: fakePodIPs{"alice": {"10.0.0.1"}, "bob": {"10.0.0.2"}}}
	return h, rl.ingest(), st
}

const runBatch = `{"repo":"owner/x","job":"issue-3","events":[{"kind":"text","time":"2026-11-16T10:00:00Z","text":"<img src=x onerror=alert(1)>"},{"kind":"tool_use","time":"","tool":"Bash","input":"{\"command\":\"ls\"}"}]}`

func TestRunEventsIngestAuth(t *testing.T) {
	_, in, st := runLogEnv(t)
	if c := postRun(t, in, "10.0.0.1", "alice", runBatch); c != 204 {
		t.Fatalf("own pod: %d", c)
	}
	if len(st.rows) != 2 || st.rows[0].Kind != "text" || st.rows[0].Agent != "alice" {
		t.Fatalf("stored: %+v", st.rows)
	}
	for _, c := range []struct {
		from, agent, body string
		code              int
	}{
		{"10.0.0.2", "alice", runBatch, 403}, // bob's pod writes alice's job
		{"10.0.0.9", "alice", runBatch, 403}, // not an agent pod
		{"10.0.0.2", "bob", runBatch, 403},   // bob's pod, alice's repository
		{"10.0.0.1", "alice", strings.Replace(runBatch, "issue-3", "../x", 1), 400},
		{"10.0.0.1", "alice", strings.Replace(runBatch, `"text","time"`, `"html","time"`, 1), 400},
		{"10.0.0.1", "alice", `{"repo":"owner/x","job":"issue-3","events":[]}`, 400},
		{"10.0.0.1", "alice", `{"repo":"owner/x","job":"issue-3","events":[{"kind":"text","time":"","x":1}]}`, 400},
		{"10.0.0.1", "Bad_Name", runBatch, 400},
	} {
		if got := postRun(t, in, c.from, c.agent, c.body); got != c.code {
			t.Errorf("%s as %s: %d, want %d", c.from, c.agent, got, c.code)
		}
	}
	if len(st.rows) != 2 {
		t.Fatalf("a refused batch was stored: %d rows", len(st.rows))
	}
}

func TestRunEventsCapPerJob(t *testing.T) {
	_, in, st := runLogEnv(t)
	ev := `{"kind":"tool_result","time":"","text":"` + strings.Repeat("a", 10000) + `"}`
	body := `{"repo":"owner/x","job":"issue-9","events":[` + strings.TrimSuffix(strings.Repeat(ev+",", 20), ",") + `]}`
	for range 20 {
		postRun(t, in, "10.0.0.1", "alice", body)
	}
	n, _ := st.JobLogBytes(context.Background(), "alice", "owner/x", "issue-9")
	if n > runLogMaxPerJob {
		t.Fatalf("over the cap: %d", n)
	}
	big := `{"repo":"owner/x","job":"issue-8","events":[{"kind":"text","time":"","text":"` + strings.Repeat("b", 20000) + `"}]}`
	if c := postRun(t, in, "10.0.0.1", "alice", big); c != 413 {
		t.Fatalf("event too large: %d", c)
	}
}

// R35: the run log has the visibility of the job (admins, write access on the repository); 404 otherwise.
func TestRunEventsVisibility(t *testing.T) {
	h, in, _ := runLogEnv(t)
	postRun(t, in, "10.0.0.1", "alice", runBatch)
	for _, c := range []struct {
		token, path string
		code        int
	}{
		{"writer", "/api/jobs/alice/issue-3/events", 200},
		{"group", "/api/jobs/alice/issue-3/events", 200},
		{"reader", "/api/jobs/alice/issue-3/events", 404}, // read only on owner/x
		{"writer", "/api/jobs/bob/issue-3/events", 404},   // another project
		{"nobody", "/api/jobs/alice/issue-3/events", 404},
		{"", "/api/jobs/alice/issue-3/events", 401},
		{"writer", "/api/jobs/alice/x/events", 400},
		{"writer", "/api/jobs/alice/issue-3/events?after=x", 400},
		{"writer", "/api/jobs/alice/issue-3/events?repo=owner/y", 404},
	} {
		if w := do(t, h, c.path, c.token); w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.token, c.path, w.Code, c.code)
		}
	}
	w := do(t, h, "/api/jobs/alice/issue-3/events", "writer")
	var r struct {
		Events []jobEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || len(r.Events) != 2 {
		t.Fatalf("events: %v %s", err, w.Body)
	}
	if !strings.Contains(string(r.Events[0].Data), `\u003cimg`) && !strings.Contains(string(r.Events[0].Data), `<img`) {
		t.Fatalf("text kept as data: %s", r.Events[0].Data)
	}
	if w := do(t, h, "/api/jobs/alice/issue-3/events?after="+jsonInt(r.Events[0].ID), "writer"); !strings.Contains(w.Body.String(), "tool_use") || strings.Contains(w.Body.String(), `"kind":"text"`) {
		t.Fatalf("after: %s", w.Body)
	}
	if w := do(t, h, "/api/jobs/alice/issue-3/events?limit=1", "writer"); !strings.Contains(w.Body.String(), "next_after") {
		t.Fatalf("paging: %s", w.Body)
	}
}

func TestRunEventsStream(t *testing.T) {
	h, in, _ := runLogEnv(t)
	postRun(t, in, "10.0.0.1", "alice", runBatch)
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/jobs/alice/issue-3/events/stream", nil)
	req.Header.Set("Authorization", "Bearer writer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for !strings.Contains(string(buf), "tool_use") {
		n, err := resp.Body.Read(tmp)
		if err != nil {
			t.Fatalf("stream: %v %s", err, buf)
		}
		buf = append(buf, tmp[:n]...)
	}
	if !strings.Contains(string(buf), "event: run-event") {
		t.Fatalf("stream: %s", buf)
	}
	req2, _ := http.NewRequest("GET", srv.URL+"/api/jobs/bob/issue-3/events/stream", nil)
	req2.Header.Set("Authorization", "Bearer writer")
	if r2, err := http.DefaultClient.Do(req2); err != nil || r2.StatusCode != 404 {
		t.Fatalf("stream of another project: %v %v", err, r2.StatusCode)
	}
}

func TestRunEventsRetentionMemory(t *testing.T) {
	st := &memJobEvents{}
	old := now
	t.Cleanup(func() { now = old })
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	now = func() time.Time { return t0.Add(-31 * 24 * time.Hour) }
	_ = st.AppendJobEvents(context.Background(), []jobEvent{{Agent: "a", Repo: "o/r", Job: "issue-1", Kind: "text", Data: []byte(`{}`)}})
	now = func() time.Time { return t0 }
	_ = st.AppendJobEvents(context.Background(), []jobEvent{{Agent: "a", Repo: "o/r", Job: "issue-1", Kind: "text", Data: []byte(`{}`)}})
	if n, _ := st.PruneJobEvents(context.Background()); n != 1 || len(st.rows) != 1 {
		t.Fatalf("prune: %d %d", n, len(st.rows))
	}
}
