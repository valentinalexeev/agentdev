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
	e := newEnv(t, "")
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
	jobsSource = func(context.Context) ([]jobCost, bool) { return nil, false }
	w = e.call("GET", "/api/costs", "writer", "")
	if !strings.Contains(w.Body.String(), `"available":false`) {
		t.Errorf("unreachable: %s", w.Body)
	}
	if w := e.call("GET", "/api/costs", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}
