package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeJobForge: one merged PR and one open PR for each repo, plus a queued issue and an issue with a pending approval.
type fakeJobForge struct{ calls int }

func (f *fakeJobForge) Pulls(_ context.Context, _, repo string) ([]forgePR, error) {
	f.calls++
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	agent := map[string]string{"owner/x": "alice", "owner/y": "bob"}[repo]
	return []forgePR{
		{Number: 10, Title: "fix " + repo, URL: "https://f/" + repo + "/pulls/10", Branch: "agent/" + agent + "/issue-1",
			State: "closed", Merged: true, Created: t0, Updated: t0.Add(time.Hour), Closed: t0.Add(time.Hour)},
		{Number: 11, Title: "open", URL: "https://f/" + repo + "/pulls/11", Branch: "agent/" + agent + "/issue-2",
			State: "open", Created: t0, Updated: t0.Add(2 * time.Hour)},
		{Number: 12, Title: "human", Branch: "feature/x", State: "open", Created: t0, Updated: t0},
	}, nil
}

func (f *fakeJobForge) AgentIssues(_ context.Context, _, repo, _ string) ([]forgeIssue, error) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return []forgeIssue{
		{Number: 2, Title: "has PR", Updated: t0},
		{Number: 3, Title: "approval pending", Updated: t0.Add(3 * time.Hour)},
		{Number: 4, Title: "queued", Updated: t0.Add(4 * time.Hour)},
	}, nil
}

func jobsEnv(t *testing.T) (http.Handler, *fakeJobForge) {
	h, wr := detailEnv(t)
	f := &fakeJobForge{}
	wr.jobs = newJobCache(f, time.Minute)
	return h, f
}

type jobsResp struct {
	Jobs       []job  `json:"jobs"`
	NextCursor string `json:"next_cursor"`
}

func getJobs(t *testing.T, h http.Handler, path, token string) jobsResp {
	t.Helper()
	w := do(t, h, path, token)
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", path, token, w.Code, w.Body)
	}
	var r jobsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJobsPhasesAndVisibility(t *testing.T) {
	h, f := jobsEnv(t)
	r := getJobs(t, h, "/api/jobs", "writer") // wendy: write on owner/x only
	phases := map[int]string{}
	for _, j := range r.Jobs {
		if j.Repo != "owner/x" || j.Agent != "alice" {
			t.Fatalf("writer sees another project's job: %+v", j)
		}
		phases[j.Issue] = j.Phase + "/" + j.Outcome
	}
	want := map[int]string{1: "done/merged", 2: "review/", 3: "build/", 4: "queued/"}
	if len(r.Jobs) != 4 || len(phases) != 4 {
		t.Fatalf("jobs: %+v", r.Jobs)
	}
	for k, v := range want {
		if phases[k] != v {
			t.Errorf("issue %d: %s, want %s", k, phases[k], v)
		}
	}
	if r.Jobs[0].Issue != 4 { // newest first
		t.Errorf("order: %+v", r.Jobs[0])
	}
	if n := len(getJobs(t, h, "/api/jobs", "nobody").Jobs); n != 0 {
		t.Errorf("nobody sees %d jobs", n)
	}
	if n := len(getJobs(t, h, "/api/jobs", "group").Jobs); n != 8 {
		t.Errorf("admin sees %d jobs, want 8", n)
	}
	if n := len(getJobs(t, h, "/api/jobs?scope=mine", "group").Jobs); n != 0 {
		t.Errorf("admin scope=mine without write access: %d", n)
	}
	if n := len(getJobs(t, h, "/api/jobs?repo=owner/y", "writer").Jobs); n != 0 {
		t.Errorf("repo filter widened visibility: %d", n)
	}
	if n := len(getJobs(t, h, "/api/jobs?repo=owner/y&phase=done", "group").Jobs); n != 1 {
		t.Errorf("repo+phase: %d", n)
	}
	before := f.calls
	getJobs(t, h, "/api/jobs", "writer")
	if f.calls != before {
		t.Errorf("forge called again within the cache TTL")
	}
	for _, bad := range []string{"?phase=x", "?scope=x", "?repo=bad", "?limit=0", "?cursor=!!"} {
		if w := do(t, h, "/api/jobs"+bad, "writer"); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
}

func TestJobsCursorPaging(t *testing.T) {
	h, _ := jobsEnv(t)
	seen := map[string]bool{}
	path, pages := "/api/jobs?limit=3", 0
	for {
		r := getJobs(t, h, path, "group")
		pages++
		for _, j := range r.Jobs {
			if seen[j.ID] {
				t.Fatalf("duplicate %s", j.ID)
			}
			seen[j.ID] = true
		}
		if r.NextCursor == "" {
			break
		}
		path = "/api/jobs?limit=3&cursor=" + r.NextCursor
	}
	if len(seen) != 8 || pages != 3 {
		t.Fatalf("paging: %d items in %d pages", len(seen), pages)
	}
}

func TestListsScopeAndCursor(t *testing.T) {
	h, _ := jobsEnv(t)
	var ag struct {
		Agents     []agent `json:"agents"`
		NextCursor string  `json:"next_cursor"`
	}
	w := do(t, h, "/api/agents?limit=1", "group")
	_ = json.Unmarshal(w.Body.Bytes(), &ag)
	if len(ag.Agents) != 1 || ag.NextCursor == "" {
		t.Fatalf("agents page 1: %s", w.Body)
	}
	w = do(t, h, "/api/agents?limit=1&cursor="+ag.NextCursor, "group")
	first := ag.Agents[0].Name
	ag.NextCursor = ""
	_ = json.Unmarshal(w.Body.Bytes(), &ag)
	if len(ag.Agents) != 1 || ag.Agents[0].Name == first || ag.NextCursor != "" {
		t.Fatalf("agents page 2: %s", w.Body)
	}
	w = do(t, h, "/api/agents?scope=mine", "reader") // rita writes on owner/y only
	if !strings.Contains(w.Body.String(), `"bob"`) || strings.Contains(w.Body.String(), `"alice"`) {
		t.Fatalf("agents scope=mine: %s", w.Body)
	}
	w = do(t, h, "/api/approvals?repo=owner/y", "group")
	if !strings.Contains(w.Body.String(), "bob-b1") || strings.Contains(w.Body.String(), "alice-a1") {
		t.Fatalf("approvals repo: %s", w.Body)
	}
	w = do(t, h, "/api/approvals?repo=owner/y", "writer")
	if strings.Contains(w.Body.String(), "bob-b1") {
		t.Fatalf("approvals: other project leaked: %s", w.Body)
	}
	w = do(t, h, "/api/approvals?limit=1", "group")
	if !strings.Contains(w.Body.String(), "next_cursor") {
		t.Fatalf("approvals cursor: %s", w.Body)
	}
}

func TestAgentDetailRecentJobs(t *testing.T) {
	h, _ := jobsEnv(t)
	w := do(t, h, "/api/agents/alice", "writer")
	var d struct {
		RecentJobs []job `json:"recentJobs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if len(d.RecentJobs) != 4 || d.RecentJobs[0].Agent != "alice" {
		t.Fatalf("recentJobs: %s", w.Body)
	}
}

func TestStreamHeartbeatAndVisibility(t *testing.T) {
	oldBeat, oldPoll := streamHeartbeat, streamPoll
	streamHeartbeat, streamPoll = 50*time.Millisecond, time.Hour
	t.Cleanup(func() { streamHeartbeat, streamPoll = oldBeat, oldPoll })
	h, _ := jobsEnv(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer writer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	types := map[string]int{}
	for sc.Scan() && types["heartbeat"] < 2 {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env sseEnvelope
		if err := json.Unmarshal([]byte(line[6:]), &env); err != nil {
			t.Fatal(err)
		}
		types[env.Type]++
		if strings.Contains(line, "owner/y") || strings.Contains(line, `"bob`) {
			t.Fatalf("stream leaked another project: %s", line)
		}
	}
	if types["hello"] != 1 || types["agents"] != 1 || types["approval"] != 1 || types["heartbeat"] < 2 {
		t.Fatalf("events: %v", types)
	}
	if w := do(t, h, "/api/stream", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}
