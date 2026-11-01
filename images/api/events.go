package main

// Security events (#70), long-term copy: the API watches SecurityEvent objects (written by the operator from the
// agent pods' log lines) and appends each one to the security_events table in the platform Postgres. The table is
// append-only (schema.sql: the API login has INSERT and SELECT only, a trigger refuses UPDATE and DELETE inside the
// retention). GET /api/security-events
// reads it. New rows go to the configured outputs (webhook.go).

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schemaV1 = "agentdev.security/v1"

// securityEvent is the agentdev.security/v1 record (docs/security-model.md, "Event schema"). The same JSON is the
// API's list item and the webhook body.
type securityEvent struct {
	ID        int64     `json:"id"`
	Schema    string    `json:"schema"`
	Time      time.Time `json:"time"`
	Agent     string    `json:"agent"`
	Job       string    `json:"job,omitempty"`
	Source    string    `json:"source"`
	Untrusted bool      `json:"untrusted,omitempty"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Action    string    `json:"action"`
	Pod       string    `json:"pod,omitempty"`
	Container string    `json:"container,omitempty"`
	Hash      string    `json:"hash"`
}

type eventFilter struct {
	Agent, Severity string
	Since           time.Time
	Before          int64 // cursor: only ids below this (0 = from the newest)
	Limit           int
}

// eventStore is the security_events table (pgStore) or an in-memory copy (tests, no database configured).
type eventStore interface {
	// Insert appends e; false when an event with the same hash is already stored. Sets e.ID.
	Insert(ctx context.Context, e *securityEvent) (bool, error)
	// List returns matching events, newest first.
	List(ctx context.Context, f eventFilter) ([]securityEvent, error)
	// Counts returns the number of events per agent since t.
	Counts(ctx context.Context, since time.Time) (map[string]int, error)
}

// cursor encodes the last id of a page; opaque to clients.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeCursor(s string) (int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("bad cursor")
	}
	return n, nil
}

// ---- Postgres ----

type pgStore struct{ db *pgxpool.Pool }

// schema.sql creates the table, the append-only trigger and the prune function. In a deployment 10-api.sh applies it
// as the owner role agentdev_owner and grants the API login (agentdev_api) only INSERT and SELECT plus EXECUTE on
// security_events_prune(); the API then cannot UPDATE, DELETE, TRUNCATE, drop the trigger or change the retention.
// Tests and a standalone API without that setup pass migrate=true and run it as their own login.
//
//go:embed schema.sql
var schemaSQL string

//go:embed grants.sql
var grantsSQL string

func newPgStore(ctx context.Context, url string, migrate bool) (*pgStore, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 30; i++ { // Postgres may start after the API
		if lastErr = db.Ping(ctx); lastErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("connect: %w", lastErr)
	}
	if migrate {
		if _, err := db.Exec(ctx, schemaSQL); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return &pgStore{db: db}, nil
}

func (s *pgStore) Insert(ctx context.Context, e *securityEvent) (bool, error) {
	err := s.db.QueryRow(ctx, `INSERT INTO security_events (schema, time, agent, job, source, untrusted, rule, severity, action, pod, container, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (hash) DO NOTHING RETURNING id`,
		e.Schema, e.Time, e.Agent, e.Job, e.Source, e.Untrusted, e.Rule, e.Severity, e.Action, e.Pod, e.Container, e.Hash).Scan(&e.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *pgStore) List(ctx context.Context, f eventFilter) ([]securityEvent, error) {
	q := `SELECT id, schema, time, agent, job, source, untrusted, rule, severity, action, pod, container, hash FROM security_events
		WHERE ($1 = '' OR agent = $1) AND ($2 = '' OR severity = $2) AND time >= $3 AND ($4 = 0 OR id < $4)
		ORDER BY id DESC LIMIT $5`
	rows, err := s.db.Query(ctx, q, f.Agent, f.Severity, f.Since, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []securityEvent{}
	for rows.Next() {
		var e securityEvent
		if err := rows.Scan(&e.ID, &e.Schema, &e.Time, &e.Agent, &e.Job, &e.Source, &e.Untrusted, &e.Rule, &e.Severity, &e.Action,
			&e.Pod, &e.Container, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) Counts(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.Query(ctx, `SELECT agent, count(*) FROM security_events WHERE time >= $1 GROUP BY agent`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

// Prune calls security_events_prune(): a SECURITY DEFINER function of the owner role that deletes rows past the
// retention stored in security_settings. The API passes no value, so it cannot shorten the retention.
func (s *pgStore) Prune(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT security_events_prune()`).Scan(&n)
	return n, err
}

// ---- in memory ----

type memStore struct {
	mu   sync.Mutex
	rows []securityEvent
	seen map[string]bool
}

func newMemStore() *memStore { return &memStore{seen: map[string]bool{}} }

func (m *memStore) Insert(_ context.Context, e *securityEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[e.Hash] {
		return false, nil
	}
	m.seen[e.Hash] = true
	e.ID = int64(len(m.rows) + 1)
	m.rows = append(m.rows, *e)
	return true, nil
}

func (m *memStore) List(_ context.Context, f eventFilter) ([]securityEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []securityEvent{}
	for i := len(m.rows) - 1; i >= 0 && len(out) < f.Limit; i-- {
		e := m.rows[i]
		if (f.Agent == "" || e.Agent == f.Agent) && (f.Severity == "" || e.Severity == f.Severity) &&
			!e.Time.Before(f.Since) && (f.Before == 0 || e.ID < f.Before) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *memStore) Counts(_ context.Context, since time.Time) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, e := range m.rows {
		if !e.Time.Before(since) {
			out[e.Agent]++
		}
	}
	return out, nil
}

// listSecurityEvents serves GET /api/security-events?agent=&severity=&since=&limit=&cursor=. since is RFC 3339 or a
// Go duration back from now (24h); limit 1..500, default 100. The response has next_cursor while more pages exist.
func listSecurityEvents(store eventStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "security events are not configured"})
			return
		}
		q := r.URL.Query()
		f := eventFilter{Agent: q.Get("agent"), Severity: q.Get("severity"), Limit: 100}
		switch f.Severity {
		case "", "low", "medium", "high":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "severity must be low, medium or high"})
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
			id, err := decodeCursor(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad cursor"})
				return
			}
			f.Before = id
		}
		// one extra row tells whether another page exists
		want := f.Limit
		f.Limit++
		list, err := store.List(r.Context(), f)
		if err != nil {
			log.Printf("list security events: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot read security events"})
			return
		}
		resp := map[string]any{"events": list}
		if len(list) > want {
			list = list[:want]
			resp["events"] = list
			resp["next_cursor"] = encodeCursor(list[len(list)-1].ID)
		}
		writeJSON(w, http.StatusOK, resp)
	})
}
