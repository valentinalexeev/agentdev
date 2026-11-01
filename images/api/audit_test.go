package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// withAuditDB installs s as the audit store for one test; set it before routes() is built.
func withAuditDB(t *testing.T, s auditStore) {
	t.Helper()
	old := auditDB
	auditDB = s
	t.Cleanup(func() { auditDB = old })
}

func TestAuditPersistedAndAdminOnly(t *testing.T) {
	m := &memAudit{}
	withAuditDB(t, m)
	e := newEnv(t, "")
	e.call("PATCH", "/api/agents/alice", "group", `{"paused":true}`)
	e.call("PATCH", "/api/agents/alice", "nobody-token", `{"paused":true}`) // 401, audited as unauthenticated
	if !strings.Contains(e.audit.String(), "AGENTDEV_AUDIT {") {
		t.Fatal("stdout audit line missing")
	}
	if len(m.rows) != 2 {
		t.Fatalf("stored %d entries, want 2", len(m.rows))
	}
	if w := e.call("GET", "/api/audit", "admin", ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin read: %d", w.Code)
	}
	if w := e.call("GET", "/api/audit", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", w.Code)
	}
	w := e.call("GET", "/api/audit?limit=1", "group", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var resp struct {
		Entries []struct {
			ID    int64      `json:"id"`
			Entry auditEntry `json:"entry"`
		} `json:"entries"`
		Next string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Entry.Result != "unauthenticated" || resp.Next == "" {
		t.Fatalf("%+v", resp)
	}
	cursor := resp.Next
	resp.Entries, resp.Next = nil, ""
	w = e.call("GET", "/api/audit?cursor="+cursor, "group", "")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Entries) != 1 ||
		resp.Entries[0].Entry.Who.Username == "" || resp.Next != "" {
		t.Fatalf("%v %+v", err, resp)
	}
	for _, q := range []string{"result=x", "limit=0", "since=x", "cursor=!"} {
		if w := e.call("GET", "/api/audit?"+q, "group", ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}

func TestAuditNoStore(t *testing.T) {
	withAuditDB(t, nil)
	e := newEnv(t, "")
	if w := e.call("GET", "/api/audit", "group", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}

// TestPgAudit needs TEST_DATABASE_URL as a superuser (like TestPgRoleSeparation): it checks the api_audit table, its
// append-only trigger and the rights of agentdev_api.
func TestPgAudit(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	su, err := newPgStore(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	setup := `DROP TABLE IF EXISTS security_events, security_settings, api_audit CASCADE;
		DROP FUNCTION IF EXISTS security_events_append_only(), security_events_prune(), api_audit_append_only(), api_audit_prune() CASCADE;
		DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner') THEN CREATE ROLE agentdev_owner NOLOGIN; END IF;
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api') THEN CREATE ROLE agentdev_api NOLOGIN; END IF; END $$;
		GRANT CREATE, USAGE ON SCHEMA public TO agentdev_owner;
		SET ROLE agentdev_owner;` + schemaSQL + schemaSQL + `RESET ROLE;` + grantsSQL // schema twice: idempotent
	if _, err := su.db.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}
	e := auditEntry{Time: time.Now().UTC().Format(time.RFC3339), Action: "patch", Agent: "pgaudit", Result: "ok",
		Who: auditWho{Username: "u"}}
	raw, _ := json.Marshal(e)
	if err := su.InsertAudit(ctx, e, raw); err != nil {
		t.Fatal(err)
	}
	l, err := su.ListAudit(ctx, auditFilter{Agent: "pgaudit", Limit: 10})
	if err != nil || len(l) != 1 || !strings.Contains(string(l[0].Entry), `"pgaudit"`) {
		t.Fatal(l, err)
	}
	// even the superuser cannot change a row inside the retention (trigger)
	for _, q := range []string{`UPDATE api_audit SET result='x'`, `DELETE FROM api_audit`} {
		if _, err := su.db.Exec(ctx, q); err == nil {
			t.Errorf("superuser may run %q", q)
		}
	}
	conn, err := su.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE agentdev_api`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `RESET ROLE`)
	if _, err := conn.Exec(ctx, `INSERT INTO api_audit (time, action, result, entry) VALUES (now(), 'patch', 'ok', '{}')`); err != nil {
		t.Fatal("insert:", err)
	}
	if _, err := conn.Exec(ctx, `SELECT count(*) FROM api_audit`); err != nil {
		t.Fatal("select:", err)
	}
	for _, q := range []string{`DELETE FROM api_audit`, `TRUNCATE api_audit`, `UPDATE api_audit SET result='y'`,
		`DROP TRIGGER api_audit_append_only ON api_audit`, `UPDATE security_settings SET retention_days=1`} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("API role may run %q", q)
		}
	}
	if _, err := conn.Exec(ctx, `SELECT api_audit_prune()`); err != nil {
		t.Fatal("prune:", err)
	}
}
