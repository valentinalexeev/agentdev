package main

// Audit trail of the write API in Postgres (#91). Every AGENTDEV_AUDIT line (write.go) is also appended to the
// api_audit table. The table follows the rules of security_events (schema.sql, grants.sql): append-only, the API
// login has INSERT and SELECT only, rows are removed only by api_audit_prune() after the retention in
// security_settings. The stdout line stays, so the cluster log pipeline keeps a second copy. GET /api/audit reads the
// table and is restricted to the admin group (ADMIN_GROUP, default agentdev-admins).

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// auditRecord is one stored audit entry; Entry is the JSON of the AGENTDEV_AUDIT line.
type auditRecord struct {
	ID    int64           `json:"id"`
	Entry json.RawMessage `json:"entry"`
}

type auditFilter struct {
	Agent, Result string
	Since         time.Time
	Before        int64 // cursor: only ids below this (0 = from the newest)
	Limit         int
}

// auditStore is the api_audit table (pgStore) or an in-memory copy (tests, no database configured).
type auditStore interface {
	InsertAudit(ctx context.Context, e auditEntry, raw []byte) error
	ListAudit(ctx context.Context, f auditFilter) ([]auditRecord, error)
}

// auditDB receives every audit entry; nil keeps the stdout line only. Set once by main before serving.
var auditDB auditStore

// persistAudit stores one entry. A database error is logged, never returned to the caller: the stdout line is
// already written, and a write that was done must not fail because the audit copy failed.
func persistAudit(e auditEntry, raw []byte) {
	if auditDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := auditDB.InsertAudit(ctx, e, raw); err != nil {
		log.Printf("store audit entry (%s %s): %v", e.Action, e.Agent, err)
	}
}

// ---- Postgres ----

func (s *pgStore) InsertAudit(ctx context.Context, e auditEntry, raw []byte) error {
	t, err := time.Parse(time.RFC3339, e.Time)
	if err != nil {
		t = now()
	}
	_, err = s.db.Exec(ctx, `INSERT INTO api_audit (time, action, agent, result, username, entry) VALUES ($1,$2,$3,$4,$5,$6)`,
		t, e.Action, e.Agent, e.Result, e.Who.Username, raw)
	return err
}

func (s *pgStore) ListAudit(ctx context.Context, f auditFilter) ([]auditRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT id, entry FROM api_audit
		WHERE ($1 = '' OR agent = $1) AND ($2 = '' OR result = $2) AND time >= $3 AND ($4 = 0 OR id < $4)
		ORDER BY id DESC LIMIT $5`, f.Agent, f.Result, f.Since, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []auditRecord{}
	for rows.Next() {
		var r auditRecord
		var b []byte
		if err := rows.Scan(&r.ID, &b); err != nil {
			return nil, err
		}
		r.Entry = b
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAudit calls api_audit_prune() (SECURITY DEFINER, owner role); the API passes no retention value.
func (s *pgStore) PruneAudit(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT api_audit_prune()`).Scan(&n)
	return n, err
}

// ---- in memory ----

type memAudit struct {
	mu   sync.Mutex
	rows []struct {
		rec auditRecord
		e   auditEntry
	}
}

func (m *memAudit) InsertAudit(_ context.Context, e auditEntry, raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, struct {
		rec auditRecord
		e   auditEntry
	}{auditRecord{ID: int64(len(m.rows) + 1), Entry: slices.Clone(raw)}, e})
	return nil
}

func (m *memAudit) ListAudit(_ context.Context, f auditFilter) ([]auditRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []auditRecord{}
	for i := len(m.rows) - 1; i >= 0 && len(out) < f.Limit; i-- {
		r := m.rows[i]
		t, _ := time.Parse(time.RFC3339, r.e.Time)
		if (f.Agent == "" || r.e.Agent == f.Agent) && (f.Result == "" || r.e.Result == f.Result) &&
			!t.Before(f.Since) && (f.Before == 0 || r.rec.ID < f.Before) {
			out = append(out, r.rec)
		}
	}
	return out, nil
}

// listAudit serves GET /api/audit?agent=&result=&since=&limit=&cursor= (same paging as /api/security-events). Only
// members of adminGroup may read it: the entries name users and show the requested changes of every agent.
func listAudit(store auditStore, adminGroup string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(ctxIdentity{}).(*identity)
		if id == nil || adminGroup == "" || !slices.Contains(id.Groups, adminGroup) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only " + adminGroup + " may read the audit trail"})
			return
		}
		if store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the audit store is not configured"})
			return
		}
		q := r.URL.Query()
		f := auditFilter{Agent: q.Get("agent"), Result: q.Get("result"), Limit: 100}
		switch f.Result {
		case "", "ok", "denied", "invalid", "error", "unauthenticated":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "result must be ok, denied, invalid, error or unauthenticated"})
			return
		}
		if v := q.Get("since"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				f.Since = t
			} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
				f.Since = time.Now().Add(-d)
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC 3339 or a duration like 24h"})
				return
			}
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1..500"})
				return
			}
			f.Limit = n
		}
		if v := q.Get("cursor"); v != "" {
			c, err := decodeCursor(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad cursor"})
				return
			}
			f.Before = c
		}
		want := f.Limit
		f.Limit++ // one extra row tells whether another page exists
		list, err := store.ListAudit(r.Context(), f)
		if err != nil {
			log.Printf("list audit: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot read the audit trail"})
			return
		}
		resp := map[string]any{"entries": list}
		if len(list) > want {
			list = list[:want]
			resp["entries"] = list
			resp["next_cursor"] = encodeCursor(list[len(list)-1].ID)
		}
		writeJSON(w, http.StatusOK, resp)
	})
}
