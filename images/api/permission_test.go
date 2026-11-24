package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func withPermission(u *unstructured.Unstructured, perm map[string]any) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(u.Object, perm, "spec", "permission")
	return u
}

func autoPerm(allow ...any) map[string]any {
	return map[string]any{"mode": "auto", "autoMode": map[string]any{"allow": allow, "softDeny": []any{"Deploy to production"}}}
}

// permEntries returns the stored "permission" entries of agent, oldest first.
func permEntries(t *testing.T, m *memAudit, agent string) []auditEntry {
	t.Helper()
	recs, _ := m.ListAudit(context.Background(), auditFilter{Agent: agent, Limit: 100})
	var out []auditEntry
	for i := len(recs) - 1; i >= 0; i-- {
		var e auditEntry
		if err := json.Unmarshal(recs[i].Entry, &e); err != nil {
			t.Fatal(err)
		}
		if e.Action == permissionAction {
			out = append(out, e)
		}
	}
	return out
}

// TestPermissionAudit (#505): a change of spec.permission makes exactly one entry with the old and new values; a
// relist, a restart of the API or a change of another field makes none.
func TestPermissionAudit(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	old := auditOut
	auditOut = io.Discard
	t.Cleanup(func() { auditOut = old })
	ctx := context.Background()
	p := &permissionAudit{}

	p.observe(ctx, agentObj("alice", "owner/x", false)) // default dontAsk: nothing to record
	if n := len(permEntries(t, m, "alice")); n != 0 {
		t.Fatalf("default made %d entries", n)
	}
	p.observe(ctx, withPermission(agentObj("alice", "owner/x", false), autoPerm("Install dependencies")))
	p.observe(ctx, withPermission(agentObj("alice", "owner/x", true), autoPerm("Install dependencies"))) // paused only
	es := permEntries(t, m, "alice")
	if len(es) != 1 {
		t.Fatalf("got %d entries, want 1", len(es))
	}
	e := es[0]
	if e.Result != "ok" || e.Before != nil || e.Reason != firstSeenReason ||
		e.After.Permission.Mode != "auto" || e.After.Permission.AutoMode.Allow[0] != "Install dependencies" ||
		e.After.Permission.AutoMode.SoftDeny[0] != "Deploy to production" || len(e.After.Permission.AutoMode.Environment) != 0 {
		t.Fatalf("%+v", e)
	}

	// a restarted API (empty cache) reads the last entry: no duplicate
	p2 := &permissionAudit{}
	p2.observe(ctx, withPermission(agentObj("alice", "owner/x", false), autoPerm("Install dependencies")))
	if n := len(permEntries(t, m, "alice")); n != 1 {
		t.Fatalf("restart made a duplicate: %d", n)
	}
	// a rule change while the API was down is recorded after the restart, with the old rules
	p3 := &permissionAudit{}
	p3.observe(ctx, withPermission(agentObj("alice", "owner/x", false), autoPerm("Install dependencies", "Run the tests")))
	es = permEntries(t, m, "alice")
	if len(es) != 2 || len(es[1].Before.Permission.AutoMode.Allow) != 1 || len(es[1].After.Permission.AutoMode.Allow) != 2 {
		t.Fatalf("%+v", es)
	}
	// back to the default
	p3.observe(ctx, agentObj("alice", "owner/x", false))
	es = permEntries(t, m, "alice")
	if len(es) != 3 || es[2].Before.Permission.Mode != "auto" || es[2].After.Permission.Mode != "dontAsk" || es[2].After.Permission.AutoMode != nil {
		t.Fatalf("%+v", es)
	}
}

// TestPermissionAuditAdminOnly: the entries are served by GET /api/audit, which only the admin group reads.
func TestPermissionAuditAdminOnly(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	e := newEnv(t, "")
	(&permissionAudit{}).observe(context.Background(), withPermission(agentObj("alice", "owner/x", false), autoPerm("x")))
	if w := e.call("GET", "/api/audit?agent=alice", "writer", ""); w.Code != http.StatusForbidden {
		t.Fatalf("writer: %d", w.Code)
	}
	w := e.call("GET", "/api/audit?agent=alice", "group", "")
	var resp struct {
		Entries []struct {
			Entry auditEntry `json:"entry"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Entries) != 1 || resp.Entries[0].Entry.Action != "permission" {
		t.Fatal(w.Code, w.Body.String())
	}
}

// TestAgentDetailPermission (#505): the admin group sees spec.permission in the agent detail; a user with write
// access on the repository does not (the rules are an admin-only spec field).
func TestAgentDetailPermission(t *testing.T) {
	h, wr := detailEnv(t)
	k := wr.store.(*kubeAgents)
	u, _ := k.Get(context.Background(), "alice")
	withPermission(u, autoPerm("Install dependencies"))
	if _, err := k.d.Resource(agentsGVR).Namespace("coding-agents").Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	get := func(token, name string) map[string]any {
		r := do(t, h, "/api/agents/"+name, token)
		if r.Code != 200 {
			t.Fatalf("%s %s: %d", token, name, r.Code)
		}
		var m map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &m)
		return m
	}
	perm, ok := get("group", "alice")["permission"].(map[string]any)
	if !ok || perm["mode"] != "auto" {
		t.Fatalf("admin: %v", perm)
	}
	am := perm["autoMode"].(map[string]any)
	if am["allow"].([]any)[0] != "Install dependencies" || len(am["environment"].([]any)) != 0 {
		t.Fatalf("rules: %v", am)
	}
	if p := get("group", "bob")["permission"].(map[string]any); p["mode"] != "dontAsk" || p["autoMode"] != nil {
		t.Fatalf("default: %v", p)
	}
	if _, ok := get("writer", "alice")["permission"]; ok {
		t.Fatal("a repository writer sees the permission rules")
	}
}

// TestPermissionAuditTwoProcesses: two API processes (two caches) on one store record one change once.
func TestPermissionAuditTwoProcesses(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	old := auditOut
	auditOut = io.Discard
	t.Cleanup(func() { auditOut = old })
	a, b := &permissionAudit{}, &permissionAudit{}
	u := withPermission(agentObj("alice", "owner/x", false), autoPerm("x"))
	var wg sync.WaitGroup
	for _, p := range []*permissionAudit{a, b, a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); p.observe(context.Background(), u.DeepCopy()) }()
	}
	wg.Wait()
	if n := len(permEntries(t, m, "alice")); n != 1 {
		t.Fatalf("first value: %d entries, want 1", n)
	}
	u2 := withPermission(agentObj("alice", "owner/x", false), autoPerm("x", "y"))
	a.observe(context.Background(), u2)
	b.observe(context.Background(), u2.DeepCopy())
	es := permEntries(t, m, "alice")
	if len(es) != 2 || len(es[1].Before.Permission.AutoMode.Allow) != 1 || len(es[1].After.Permission.AutoMode.Allow) != 2 {
		t.Fatalf("%+v", es)
	}
}

// TestPermissionAuditStdout: the AGENTDEV_AUDIT line has the mode, the rule counts and a sha256, never the rule text;
// the stored entry keeps the full rules.
func TestPermissionAuditStdout(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	buf := &bytes.Buffer{}
	old := auditOut
	auditOut = buf
	t.Cleanup(func() { auditOut = old })
	p := &permissionAudit{}
	p.observe(context.Background(), withPermission(agentObj("alice", "owner/x", false), autoPerm("SECRET-RULE-ONE")))
	p.observe(context.Background(), withPermission(agentObj("alice", "owner/x", false), autoPerm("SECRET-RULE-TWO")))
	out := buf.String()
	if strings.Count(out, "AGENTDEV_AUDIT ") != 2 {
		t.Fatalf("want 2 lines: %s", out)
	}
	if strings.Contains(out, "SECRET-RULE") || strings.Contains(out, "Deploy to production") {
		t.Fatalf("rule text on stdout: %s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var l struct {
		Action string `json:"action"`
		Before struct {
			Permission struct {
				Mode  string         `json:"mode"`
				Rules map[string]any `json:"rules"`
			} `json:"permission"`
		} `json:"before"`
		After struct {
			Permission struct {
				Mode  string         `json:"mode"`
				Rules map[string]any `json:"rules"`
			} `json:"permission"`
		} `json:"after"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "AGENTDEV_AUDIT ")), &l); err != nil {
		t.Fatal(err)
	}
	if l.Action != "permission" || l.After.Permission.Mode != "auto" || l.After.Permission.Rules["allow"] != float64(1) ||
		len(l.After.Permission.Rules["sha256"].(string)) != 64 || l.Before.Permission.Rules["sha256"] == l.After.Permission.Rules["sha256"] {
		t.Fatalf("%+v", l)
	}
	es := permEntries(t, m, "alice")
	if len(es) != 2 || es[1].After.Permission.AutoMode.Allow[0] != "SECRET-RULE-TWO" {
		t.Fatalf("stored entry lost the rules: %+v", es)
	}
}

// TestPermissionAuditFirstSeen: no earlier entry (a pruned table, a restarted in-memory store) and a value other than
// the default: before null, reason "first seen"; the default makes no entry.
func TestPermissionAuditFirstSeen(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	old := auditOut
	auditOut = io.Discard
	t.Cleanup(func() { auditOut = old })
	p := &permissionAudit{}
	p.observe(context.Background(), agentObj("bob", "owner/y", false))
	if n := len(permEntries(t, m, "bob")); n != 0 {
		t.Fatalf("default: %d entries", n)
	}
	p.observe(context.Background(), withPermission(agentObj("carol", "owner/z", false), map[string]any{"mode": "auto"}))
	es := permEntries(t, m, "carol")
	if len(es) != 1 || es[0].Before != nil || es[0].Reason != firstSeenReason || es[0].After.Permission.Mode != "auto" {
		t.Fatalf("%+v", es)
	}
	raw, _ := m.ListAudit(context.Background(), auditFilter{Agent: "carol", Limit: 1})
	if !strings.Contains(string(raw[0].Entry), `"before":null`) {
		t.Fatal(string(raw[0].Entry))
	}
}
