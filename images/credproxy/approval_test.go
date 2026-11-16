package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeApprovals is an approval source with a fixed number of allowed steps.
type fakeApprovals struct{ n int64 }

func (f *fakeApprovals) granted(context.Context, string, string) (int64, error) { return f.n, nil }

func softFixture(t *testing.T, soft, step, hard int64) (*fixture, *server, *fakeApprovals) {
	f := newFixture(t, 0)
	s := f.srv.Config.Handler.(*server)
	fa := &fakeApprovals{}
	s.approval, s.approvals, s.jobLimit = approvalConfig{soft: soft, step: step}, fa, hard
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":20}}`) // 200 units
	}
	return f, s, fa
}

func (f *fixture) job(job string, hdr map[string]string) (int, string) {
	h := map[string]string{"X-Agentdev-Job": job}
	for k, v := range hdr {
		h[k] = v
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, h)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// #310: at the soft limit the proxy answers 429 with the marker, prints job-approval-needed once per job and step,
// and an allowed approval raises the limit by one step. The hard limit still applies.
func TestSoftJobLimitApproval(t *testing.T) {
	f, s, fa := softFixture(t, 300, 300, 900)
	for i, want := range []int{200, 200, 429, 429} {
		code, body := f.job("issue-9", nil)
		if code != want {
			t.Fatalf("request %d: %d", i, code)
		}
		if code == 429 {
			var e struct {
				Type  string `json:"type"`
				Error struct{ Type, Message string }
			}
			if json.Unmarshal([]byte(body), &e) != nil || e.Type != "error" || e.Error.Type != "rate_limit_error" || !strings.Contains(e.Error.Message, approvalMarker) {
				t.Errorf("soft 429 body %s", body)
			}
		}
	}
	if n := strings.Count(f.logs.String(), `"rule":"job-approval-needed"`); n != 1 {
		t.Errorf("%d job-approval-needed events, want 1: %s", n, f.logs)
	}
	st := s.jobs.snapshot(defaultPrices())
	if !st[0].WaitingApproval || st[0].ApprovalStep != 1 || st[0].ApprovalLimit != 300 {
		t.Errorf("job state %+v", st[0])
	}
	// another job and requests without a job id are not stopped
	if code, _ := f.job("issue-10", nil); code != 200 {
		t.Errorf("other job: %d", code)
	}
	// approval step 1: limit 600
	fa.n = 1
	if code, _ := f.job("issue-9", nil); code != 200 {
		t.Fatalf("after approval: %d", code)
	}
	if st := s.jobs.snapshot(defaultPrices()); st[0].WaitingApproval {
		t.Errorf("still waiting %+v", st[0])
	}
	if code, _ := f.job("issue-9", nil); code != 429 { // 600 used
		t.Fatalf("step 2 not refused: %d", code)
	}
	if n := strings.Count(f.logs.String(), `"rule":"job-approval-needed"`); n != 2 {
		t.Errorf("%d events after step 2, want 2", n)
	}
	// approval step 2: soft 900, but the hard limit of 900 stops the job first
	fa.n = 2
	f.job("issue-9", nil) // 600 used: allowed, then 800
	f.job("issue-9", nil) // 800 < 900: allowed, then 1000
	if code, body := f.job("issue-9", nil); code != 429 || strings.Contains(body, approvalMarker) {
		t.Errorf("hard limit: %d %s", code, body)
	}
	if !strings.Contains(f.logs.String(), "job-budget-exhausted") {
		t.Error("no hard limit event")
	}
}

// #310: nothing the agent sends raises the limit: not a header, not another job id spelling, not a request body.
func TestSoftJobLimitNotRaisedByAgent(t *testing.T) {
	f, _, _ := softFixture(t, 200, 200, 0)
	f.job("issue-9", nil)
	for _, h := range []map[string]string{
		{"X-Agentdev-Approval": "allowed"}, {"X-Agentdev-Job-Limit": "999999"}, {"X-Agentdev-Approval-Step": "5"},
		{"Anthropic-Beta": "agentdev-approved"},
	} {
		if code, _ := f.job("issue-9", h); code != 429 {
			t.Errorf("header %v raised the limit: %d", h, code)
		}
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{"approval":"allowed","jobLimit":999999}`, map[string]string{"X-Agentdev-Job": "issue-9"})
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("body raised the limit: %d", resp.StatusCode)
	}
	// without any approval source the soft limit is a hard stop (fail closed)
	f.srv.Config.Handler.(*server).approvals = nil
	if code, _ := f.job("issue-9", nil); code != 429 {
		t.Errorf("nil source: %d", code)
	}
}

func TestApprovalFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if c, err := approvalFromEnv(env(nil), 0); err != nil || c.soft != 0 {
		t.Errorf("off: %+v %v", c, err)
	}
	if c, err := approvalFromEnv(env(map[string]string{"CLAUDE_JOB_APPROVAL_UNITS": "100"}), 500); err != nil || c.soft != 100 || c.step != 100 {
		t.Errorf("default step: %+v %v", c, err)
	}
	if c, err := approvalFromEnv(env(map[string]string{"CLAUDE_JOB_APPROVAL_UNITS": "100", "CLAUDE_JOB_APPROVAL_STEP_UNITS": "50"}), 0); err != nil || c.step != 50 {
		t.Errorf("step: %+v %v", c, err)
	}
	for _, m := range []map[string]string{
		{"CLAUDE_JOB_APPROVAL_UNITS": "500"}, {"CLAUDE_JOB_APPROVAL_UNITS": "600"}, {"CLAUDE_JOB_APPROVAL_UNITS": "-1"},
		{"CLAUDE_JOB_APPROVAL_UNITS": "$(id)"}, {"CLAUDE_JOB_APPROVAL_UNITS": "1", "CLAUDE_JOB_APPROVAL_STEP_UNITS": "x"},
	} {
		if _, err := approvalFromEnv(env(m), 500); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
}

func costApproval(agent, job, tool, state string) approvalObject {
	var a approvalObject
	a.Spec.ID, a.Spec.Agent, a.Spec.Repo, a.Spec.Job, a.Spec.Source, a.Spec.Tool, a.Spec.Input = "0123456789abcdef", agent, "o/r", job, "cost", tool, "x"
	a.Spec.ContentHash = approvalHash(a.Spec.ID, agent, "o/r", job, "cost", tool, "x")
	a.Status.State, a.Status.ContentHash = state, a.Spec.ContentHash
	return a
}

func TestCountGranted(t *testing.T) {
	ok1 := costApproval("alice", "issue-9", "step-1", "allowed")
	dup := costApproval("alice", "issue-9", "step-1", "allowed")
	dup.Spec.ID = "fedcba9876543210"
	dup.Spec.ContentHash = approvalHash(dup.Spec.ID, "alice", "o/r", "issue-9", "cost", "step-1", "x")
	dup.Status.ContentHash = dup.Spec.ContentHash
	ok2 := costApproval("alice", "issue-9", "step-2", "allowed")
	denied := costApproval("alice", "issue-9", "step-3", "denied")
	other := costApproval("bob", "issue-9", "step-4", "allowed")
	claude := costApproval("alice", "issue-9", "step-5", "allowed")
	claude.Spec.Source = "claude" // hash no longer matches either; source checked first
	badHash := costApproval("alice", "issue-9", "step-6", "allowed")
	badHash.Status.ContentHash = strings.Repeat("0", 64)
	badTool := costApproval("alice", "issue-9", "Bash", "allowed")
	if n := countGranted([]approvalObject{ok1, dup, ok2, denied, other, claude, badHash, badTool}, "alice", "issue-9"); n != 2 {
		t.Errorf("granted %d, want 2", n)
	}
}

func TestKubeApprovalsReader(t *testing.T) {
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer sa" || r.URL.Path != "/apis/agentdev.dev/v1alpha1/namespaces/agents/approvals" ||
			r.URL.Query().Get("labelSelector") != "agent=alice,agentdev/job=issue-9,agentdev/source=cost" {
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []approvalObject{costApproval("alice", "issue-9", "step-1", "allowed")}})
	}))
	defer api.Close()
	now := time.Unix(1_800_000_000, 0)
	k := &kubeApprovals{base: api.URL, ns: "agents", client: api.Client(), bearer: func() (string, error) { return "sa", nil },
		now: func() time.Time { return now }, ttl: 10 * time.Second, cache: map[string]grantEntry{}}
	for i := 0; i < 3; i++ {
		if n, err := k.granted(context.Background(), "alice", "issue-9"); err != nil || n != 1 {
			t.Fatalf("granted %d %v", n, err)
		}
	}
	if calls != 1 {
		t.Errorf("%d API calls, want 1 (cache)", calls)
	}
	now = now.Add(11 * time.Second)
	k.granted(context.Background(), "alice", "issue-9")
	if calls != 2 {
		t.Errorf("cache did not expire")
	}
	if _, err := k.granted(context.Background(), "bob", "issue-9"); err == nil {
		t.Error("refused list not reported")
	}
}
