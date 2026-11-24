package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	if e.Result != "ok" || e.Before.Permission.Mode != "dontAsk" || e.Before.Permission.AutoMode != nil ||
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
