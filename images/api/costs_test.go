package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAgentBudgetEdit(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	_, _ = e.k.c.CoreV1().ConfigMaps("coding-agents").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyBudgetsCM, Namespace: "coding-agents"}, Data: map[string]string{"bob": "7"}}, metav1.CreateOptions{})
	var got []securityEvent
	wr := &writer{store: e.k, perms: newGiteaPerms("", "svc"), adminGroup: "agentdev-admins", usage: e.k,
		events: func(_ context.Context, ev securityEvent) { got = append(got, ev) }}
	e.h = routes(idVerify, e.k, nil, wr)
	cm := func() map[string]string {
		c, _ := e.k.c.CoreV1().ConfigMaps("coding-agents").Get(ctx, proxyBudgetsCM, metav1.GetOptions{})
		return c.Data
	}
	// not an admin: refused, nothing written
	if w := e.call("PUT", "/api/costs/budgets/alice", "writer", `{"units":500000}`); w.Code != http.StatusForbidden || cm()["alice"] != "" {
		t.Fatalf("writer: %d %v", w.Code, cm())
	}
	for _, bad := range []string{`{"units":-1}`, `{"units":1.5}`, `{"units":"5"}`, `{"units":1e9}`, `{"units":5,"x":1}`, `nope`} {
		if w := e.call("PUT", "/api/costs/budgets/alice", "group", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := e.call("PUT", "/api/costs/budgets/Bad..Name", "group", `{"units":5}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", w.Code)
	}
	if w := e.call("PUT", "/api/costs/budgets/alice", "group", `{"units":500000}`); w.Code != http.StatusOK || cm()["alice"] != "500000" {
		t.Fatalf("admin set: %d %v", w.Code, cm())
	}
	if len(got) != 1 || got[0].Rule != "budget-changed" || got[0].Agent != "alice" || got[0].Hash == "" || !strings.Contains(got[0].Action, "500000") {
		t.Errorf("event %+v", got)
	}
	if w := e.call("PUT", "/api/costs/budgets/bob", "group", `{"units":null}`); w.Code != http.StatusOK || cm()["bob"] != "" || cm()["alice"] != "500000" {
		t.Fatalf("admin reset: %d %v", w.Code, cm())
	}
	if !strings.Contains(e.audit.String(), `"action":"set-budget"`) || !strings.Contains(e.audit.String(), `"result":"denied"`) || !strings.Contains(e.audit.String(), `"before":"7"`) {
		t.Errorf("audit %s", e.audit)
	}
	if w := e.call("PUT", "/api/costs/budgets/alice", "", `{"units":1}`); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}

func TestCostsJobs(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	old := jobsSource
	defer func() { jobsSource = old }()
	usd := 0.25
	jobsSource = func(context.Context) ([]jobCost, bool) {
		return []jobCost{
			{Agent: "alice", Job: "issue-3", Units: 900, USD: &usd, Tokens: map[string]int64{"cache_read": 5000, "<script>": 1}, Requests: 4},
			{Agent: "alice", Job: "BAD JOB", Units: 1},
			{Agent: "alice", Job: "pr-4", Units: -5},
		}, true
	}
	w := e.call("GET", "/api/costs", "writer", "")
	var out struct {
		Available bool
		Jobs      []jobCost
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if !out.Available || len(out.Jobs) != 1 || out.Jobs[0].Job != "issue-3" || *out.Jobs[0].USD != 0.25 || len(out.Jobs[0].Tokens) != 1 {
		t.Errorf("costs %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `"attribution":"agent-reported"`) {
		t.Errorf("attribution missing: %s", w.Body)
	}
	jobsSource = func(context.Context) ([]jobCost, bool) { return nil, false }
	w = e.call("GET", "/api/costs", "writer", "")
	if !strings.Contains(w.Body.String(), `"available":false`) {
		t.Errorf("unreachable: %s", w.Body)
	}
	if w := e.call("GET", "/api/costs", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}

// Review of #279: job ids are issue and PR numbers; a user sees only jobs of agents whose repository it can read.
func TestCostsVisibility(t *testing.T) {
	gitea := fakeGitea(t) // wendy write, rita read, adam admin on owner/x; gina only in the admin group
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	if _, err := e.k.d.Resource(agentsGVR).Namespace("coding-agents").Create(context.Background(), agentObj("secret-agent", "private/repo", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	old := jobsSource
	defer func() { jobsSource = old }()
	jobsSource = func(context.Context) ([]jobCost, bool) {
		return []jobCost{{Agent: "alice", Job: "issue-3", Units: 9}, {Agent: "secret-agent", Job: "issue-77", Units: 9},
			{Agent: "ghost", Job: "issue-1", Units: 9}}, true
	}
	jobsOf := func(token string) string {
		var out struct{ Jobs []jobCost }
		w := e.call("GET", "/api/costs", token, "")
		json.Unmarshal(w.Body.Bytes(), &out)
		var s []string
		for _, j := range out.Jobs {
			s = append(s, j.Agent+"/"+j.Job)
		}
		return strings.Join(s, ",")
	}
	if got := jobsOf("reader"); got != "alice/issue-3" {
		t.Errorf("reader sees %q", got)
	}
	if got := jobsOf("group"); got != "alice/issue-3,secret-agent/issue-77,ghost/issue-1" {
		t.Errorf("admin sees %q", got)
	}
	e2 := newEnv(t, "http://127.0.0.1:1") // forge down: nothing, not everything
	if w := e2.call("GET", "/api/costs", "reader", ""); strings.Contains(w.Body.String(), "issue-") {
		t.Errorf("forge down leaked jobs: %s", w.Body)
	}
}

func TestAgentBudgetUnknownAgent(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	_, _ = e.k.c.CoreV1().ConfigMaps("coding-agents").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyBudgetsCM, Namespace: "coding-agents"}}, metav1.CreateOptions{})
	if w := e.call("PUT", "/api/costs/budgets/alcie", "group", `{"units":5}`); w.Code != http.StatusNotFound {
		t.Errorf("typo accepted: %d", w.Code)
	}
	c, _ := e.k.c.CoreV1().ConfigMaps("coding-agents").Get(ctx, proxyBudgetsCM, metav1.GetOptions{})
	if len(c.Data) != 0 {
		t.Errorf("dead key written: %v", c.Data)
	}
}
