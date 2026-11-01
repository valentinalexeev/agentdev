package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// UI decisions (#67): only a human web session with write access (or the admin group) decides.
func uiDecideEnv(t *testing.T) (http.Handler, *kubeAgents, *strings.Builder, *memAudit) {
	t.Helper()
	gitea := fakeGitea(t)
	t.Cleanup(gitea.Close)
	k, buf := approvalEnv(t, agentObj("carol", "owner/x", false), approvalObj("carol", aid, "owner/x", t0))
	old := auditDB
	mem := &memAudit{}
	auditDB = mem
	t.Cleanup(func() { auditDB = old })
	testIDs["agent-session"] = &identity{Subject: "s-c", Username: "carol", ForgeLogin: "carol"}
	t.Cleanup(func() { delete(testIDs, "agent-session") })
	wr := &writer{store: k, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"}
	return routes(idVerify, k, nil, wr), k, buf, mem
}

func postDecision(h http.Handler, token, header, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/approvals/carol-"+aid+"/decision", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if header != "" {
		r.Header.Set("X-Approval-Secret", header)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func body(decision string) string {
	return `{"decision":"` + decision + `","contentHash":"` + strings.Repeat("ab", 32) + `"}`
}

func TestUIDecisionRefusesAgents(t *testing.T) {
	h, k, buf, _ := uiDecideEnv(t)
	// an agent's own credentials are not web sessions: the request secret, a forge token as bearer, no bearer
	if w := postDecision(h, "", testSecret, body("approve")); w.Code != http.StatusUnauthorized {
		t.Fatalf("approval secret: %d", w.Code)
	}
	if w := postDecision(h, testSecret, "", body("approve")); w.Code != http.StatusUnauthorized {
		t.Fatalf("agent token as bearer: %d", w.Code)
	}
	// a session whose forge login is an agent's
	if w := postDecision(h, "agent-session", "", body("approve")); w.Code != http.StatusForbidden {
		t.Fatalf("agent session: %d %s", w.Code, w.Body)
	}
	// a platform account
	approvalDenyLogins["wendy"] = true
	w := postDecision(h, "writer", "", body("approve"))
	delete(approvalDenyLogins, "wendy")
	if w.Code != http.StatusForbidden {
		t.Fatalf("platform account: %d", w.Code)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("decided by a refused caller: %+v", a)
	}
	if !strings.Contains(buf.String(), `"result":"unauthenticated"`) || !strings.Contains(buf.String(), "never decide") {
		t.Fatalf("refusals not audited: %s", buf)
	}
}

func TestUIDecisionRefusesNonApprover(t *testing.T) {
	h, k, _, _ := uiDecideEnv(t)
	if w := postDecision(h, "reader", "", body("approve")); w.Code != http.StatusNotFound {
		t.Fatalf("read-only user: %d %s", w.Code, w.Body) // not visible: not revealed
	}
	if w := postDecision(h, "nobody", "", body("deny")); w.Code != http.StatusNotFound {
		t.Fatalf("no forge login: %d", w.Code)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("decided by a non-approver: %+v", a)
	}
}

func TestUIDecisionIdempotentAndAudited(t *testing.T) {
	h, k, _, mem := uiDecideEnv(t)
	if w := postDecision(h, "writer", "", `{"decision":"approve","contentHash":"bad"}`); w.Code != http.StatusConflict {
		t.Fatalf("stale hash: %d", w.Code)
	}
	if w := postDecision(h, "writer", "", body("bogus")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad decision: %d", w.Code)
	}
	if w := postDecision(h, "writer", "", body("approve-job")); w.Code != 200 {
		t.Fatalf("decide: %d %s", w.Code, w.Body)
	}
	a := status(t, k, "carol-"+aid)
	if a.State != "allowed" || a.Scope != "job" || a.DecidedBy != "wendy" || a.DecidedVia != "ui" || a.DecidedHash != strings.Repeat("ab", 32) {
		t.Fatalf("record: %+v", a)
	}
	rv := a.rv
	// the same decision again (double click, second tab): 200, nothing changes
	w := postDecision(h, "group", "", body("approve-job"))
	if w.Code != 200 {
		t.Fatalf("repeat: %d %s", w.Code, w.Body)
	}
	var got approval
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if a2 := status(t, k, "carol-"+aid); a2.rv != rv || a2.DecidedBy != "wendy" || got.DecidedBy != "wendy" {
		t.Fatalf("repeat changed the record: %+v", a2)
	}
	// a different decision after the first: refused
	if w := postDecision(h, "admin", "", body("deny")); w.Code != http.StatusConflict {
		t.Fatalf("second decision: %d", w.Code)
	}
	if a3 := status(t, k, "carol-"+aid); a3.State != "allowed" {
		t.Fatalf("overwritten: %+v", a3)
	}
	// Postgres audit copy names who decided
	recs, _ := mem.ListAudit(context.Background(), auditFilter{Limit: 100})
	found := false
	for _, r := range recs {
		var e auditEntry
		_ = json.Unmarshal(r.Entry, &e)
		if e.Action == "approval-allowed" && e.Result == "ok" && e.Who.Username == "wendy" && e.Who.ForgeUser == "wendy" && e.Grant == "repo-write" {
			found = true
		}
	}
	if !found {
		t.Fatalf("decision not in the audit store: %d records", len(recs))
	}
}

func TestUIDecisionAdminGroupDenies(t *testing.T) {
	h, k, _, _ := uiDecideEnv(t)
	if w := postDecision(h, "group", "", body("deny")); w.Code != 200 {
		t.Fatalf("admin group: %d %s", w.Code, w.Body)
	}
	if a := status(t, k, "carol-"+aid); a.State != "denied" || a.DecidedBy != "gina" || a.Scope != "" {
		t.Fatalf("record: %+v", a)
	}
}

// A write that keeps conflicting (another writer touches the object each time) answers 409, not 502: nothing was
// written and the caller reloads (#138 review).
func TestUIDecisionRepeatedConflictIs409(t *testing.T) {
	_, k, _, _ := uiDecideEnv(t)
	gitea := fakeGitea(t)
	t.Cleanup(gitea.Close)
	wr := &writer{store: conflictStore{k}, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"}
	h := routes(idVerify, k, nil, wr)
	if w := postDecision(h, "writer", "", body("approve")); w.Code != http.StatusConflict {
		t.Fatalf("repeated conflict: %d %s", w.Code, w.Body)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("written despite conflicts: %+v", a)
	}
}
