package main

// Chat sessions (#346): storage of sessions and transcripts. Postgres tables chat_sessions and chat_events
// (schema.sql); the API login may INSERT and SELECT events (append-only, like api_audit), INSERT, SELECT and UPDATE
// sessions, and call chat_prune(), which removes sessions and events older than chat_settings.retention_days
// (CHAT_RETENTION_DAYS, default 30). Without a database everything stays in memory (dev, tests).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// chatSession is one row of chat_sessions.
type chatSession struct {
	ID          string     `json:"id"`
	Agent       string     `json:"agent"`
	Repo        string     `json:"repo"`
	Forge       string     `json:"forge"`
	StartedBy   string     `json:"startedBy"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	EndedReason string     `json:"endedReason,omitempty"`
	LastSeq     int64      `json:"lastSeq"`
}

// chatEvent is one event of a session. Seq > 0: from the session server (its order); seq 0: written by the API
// (issue-created, start-refused and the like). ID is the store's order and the SSE cursor.
type chatEvent struct {
	ID      int64           `json:"id"`
	Session string          `json:"session"`
	Seq     int64           `json:"seq"`
	Time    time.Time       `json:"time"`
	Type    string          `json:"type"`
	Author  string          `json:"author,omitempty"`
	Data    json.RawMessage `json:"data"`
}

var errChatNotFound = errors.New("no such session")

type chatStore interface {
	CreateSession(ctx context.Context, s chatSession) error
	GetSession(ctx context.Context, id string) (*chatSession, error)
	ListSessions(ctx context.Context, since time.Time) ([]chatSession, error)
	EndSession(ctx context.Context, id, reason string, at time.Time) error
	// AppendEvents stores events (duplicates of session+seq>0 are ignored) and raises last_seq to upTo, also over
	// events that were dropped, so they are not read again.
	AppendEvents(ctx context.Context, session string, evs []chatEvent, upTo int64) error
	// Events gives events with ID > after, oldest first, at most limit.
	Events(ctx context.Context, session string, after int64, limit int) ([]chatEvent, error)
	PruneChat(ctx context.Context) (int64, error)
}

// ---- memory ----

type memChat struct {
	mu       sync.Mutex
	sessions map[string]*chatSession
	events   []chatEvent
	seen     map[string]bool
	nextID   int64
}

func newMemChat() *memChat {
	return &memChat{sessions: map[string]*chatSession{}, seen: map[string]bool{}}
}

func (m *memChat) CreateSession(_ context.Context, s chatSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return errors.New("session exists")
	}
	c := s
	m.sessions[s.ID] = &c
	return nil
}

func (m *memChat) GetSession(_ context.Context, id string) (*chatSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, errChatNotFound
	}
	c := *s
	return &c, nil
}

func (m *memChat) ListSessions(_ context.Context, since time.Time) ([]chatSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []chatSession{}
	for _, s := range m.sessions {
		if !s.StartedAt.Before(since) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (m *memChat) EndSession(_ context.Context, id, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return errChatNotFound
	}
	if s.EndedAt == nil {
		s.EndedAt, s.EndedReason = &at, reason
	}
	return nil
}

func (m *memChat) AppendEvents(_ context.Context, session string, evs []chatEvent, upTo int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[session]; s != nil && upTo > s.LastSeq {
		s.LastSeq = upTo
	}
	for _, e := range evs {
		e.Session = session
		if e.Seq > 0 {
			k := e.Session + "\x00" + itoa(e.Seq)
			if m.seen[k] {
				continue
			}
			m.seen[k] = true
		}
		m.nextID++
		e.ID = m.nextID
		m.events = append(m.events, e)
	}
	return nil
}

func (m *memChat) Events(_ context.Context, session string, after int64, limit int) ([]chatEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []chatEvent{}
	for _, e := range m.events {
		if e.Session == session && e.ID > after {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memChat) PruneChat(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := now().Add(-time.Duration(chatRetentionDays+1) * 24 * time.Hour)
	var n int64
	for id, s := range m.sessions {
		if s.StartedAt.Before(cut) && s.EndedAt != nil {
			delete(m.sessions, id)
			n++
		}
	}
	keep := m.events[:0]
	for _, e := range m.events {
		if _, ok := m.sessions[e.Session]; ok {
			keep = append(keep, e)
		}
	}
	m.events = keep
	return n, nil
}

// chatRetentionDays is used by the memory store only; Postgres reads chat_settings.
var chatRetentionDays = 30

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ---- Postgres ----

func (s *pgStore) CreateSession(ctx context.Context, c chatSession) error {
	_, err := s.db.Exec(ctx, `INSERT INTO chat_sessions (id, agent, repo, forge, started_by, started_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, c.ID, c.Agent, c.Repo, c.Forge, c.StartedBy, c.StartedAt)
	return err
}

const chatSessionCols = `id, agent, repo, forge, started_by, started_at, ended_at, ended_reason, last_seq`

func scanChatSession(row pgx.Row) (*chatSession, error) {
	var c chatSession
	if err := row.Scan(&c.ID, &c.Agent, &c.Repo, &c.Forge, &c.StartedBy, &c.StartedAt, &c.EndedAt, &c.EndedReason, &c.LastSeq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errChatNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (s *pgStore) GetSession(ctx context.Context, id string) (*chatSession, error) {
	return scanChatSession(s.db.QueryRow(ctx, `SELECT `+chatSessionCols+` FROM chat_sessions WHERE id = $1`, id))
}

func (s *pgStore) ListSessions(ctx context.Context, since time.Time) ([]chatSession, error) {
	rows, err := s.db.Query(ctx, `SELECT `+chatSessionCols+` FROM chat_sessions WHERE started_at >= $1 ORDER BY started_at DESC LIMIT 1000`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chatSession{}
	for rows.Next() {
		c, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *pgStore) EndSession(ctx context.Context, id, reason string, at time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE chat_sessions SET ended_at = $2, ended_reason = $3 WHERE id = $1 AND ended_at IS NULL`, id, at, reason)
	return err
}

func (s *pgStore) AppendEvents(ctx context.Context, session string, evs []chatEvent, upTo int64) error {
	if len(evs) == 0 && upTo == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, e := range evs {
		if _, err := tx.Exec(ctx, `INSERT INTO chat_events (session, seq, time, type, author, data) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (session, seq) WHERE seq > 0 DO NOTHING`, session, e.Seq, e.Time, e.Type, e.Author, []byte(e.Data)); err != nil {
			return err
		}
	}
	if upTo > 0 {
		if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET last_seq = $2 WHERE id = $1 AND last_seq < $2`, session, upTo); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *pgStore) Events(ctx context.Context, session string, after int64, limit int) ([]chatEvent, error) {
	rows, err := s.db.Query(ctx, `SELECT id, session, seq, time, type, author, data FROM chat_events
		WHERE session = $1 AND id > $2 ORDER BY id LIMIT $3`, session, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chatEvent{}
	for rows.Next() {
		var e chatEvent
		var b []byte
		if err := rows.Scan(&e.ID, &e.Session, &e.Seq, &e.Time, &e.Type, &e.Author, &b); err != nil {
			return nil, err
		}
		e.Data = b
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) PruneChat(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT chat_prune()`).Scan(&n)
	return n, err
}
