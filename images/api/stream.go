package main

// Live updates (#345): GET /api/stream, server-sent events for the console. It replaces the 15 s poll.
//
// Envelope (also for the chat session events of #346, which reuse sseConn): every message is
//
//	event: <type>
//	id: <sequence number in this connection>
//	data: {"type": "<type>", "time": "<RFC 3339>", "data": <payload>}
//
// Types: hello (once, data {"heartbeatSeconds": n}), heartbeat (data null), agents (data: the visible agent list, as
// GET /api/agents), approval (data: one approval, as GET /api/approvals/{name}; sent on create and on each change),
// approval-removed (data {"name"}), security-event (data: one event, as GET /api/security-events), usage (data null:
// the usage changed, read GET /api/usage again), bye (data {"reason"}: the server ends the stream; reconnect).
//
// Visibility is the same as for the lists: every item is checked against the caller's identity with the rules of its
// list (admin group, or write access on the repository). The caller's forge permissions are read again every 5
// minutes, and the stream ends after 30 minutes so the browser reconnects with a fresh token. The stream reads the
// cluster every 2 seconds (approvals, agents, usage) and the event store for new rows; it never reads agent output.
// The same auth applies as for every /api route (bearer token); EventSource cannot set headers, so the UI reads the
// stream with fetch() and a ReadableStream.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	streamPoll      = 2 * time.Second
	streamHeartbeat = 15 * time.Second
	streamMaxAge    = 30 * time.Minute
	streamPermTTL   = 5 * time.Minute
	streamPerUser   = 5 // concurrent streams per OIDC subject
)

// sseConn writes server-sent events with the envelope above. Shared with other streaming endpoints (#346).
type sseConn struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	seq int64
}

func newSSE(w http.ResponseWriter) (*sseConn, error) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx in the UI pod must not buffer the stream
	w.WriteHeader(http.StatusOK)
	c := &sseConn{w: w, rc: rc}
	if err := c.flush(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *sseConn) flush() error {
	// the server's WriteTimeout would cut a long stream: each write gets its own deadline
	_ = c.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.rc.Flush()
}

type sseEnvelope struct {
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	Data any       `json:"data"`
}

func (c *sseConn) send(typ string, data any) error {
	b, err := json.Marshal(sseEnvelope{Type: typ, Time: time.Now().UTC(), Data: data})
	if err != nil {
		return err
	}
	c.seq++
	_ = c.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(c.w, "event: %s\nid: %d\ndata: %s\n\n", typ, c.seq, b); err != nil {
		return err
	}
	return c.flush()
}

// streamLimiter caps the open streams per subject.
type streamLimiter struct {
	mu sync.Mutex
	n  map[string]int
}

func (l *streamLimiter) acquire(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = map[string]int{}
	}
	if l.n[sub] >= streamPerUser {
		return false
	}
	l.n[sub]++
	return true
}

func (l *streamLimiter) release(sub string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n[sub]--
}

var streams streamLimiter

// streamState is what one connection has already sent.
type streamState struct {
	agents    [32]byte
	approvals map[string][32]byte
	lastEvent int64
	usage     [32]byte
	usageSeen bool
}

func hashJSON(v any) [32]byte {
	b, _ := json.Marshal(v)
	return sha256.Sum256(b)
}

func (w *writer) stream() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(ctxIdentity{}).(*identity)
		if id == nil {
			writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		if !streams.acquire(id.Subject) {
			writeJSON(rw, http.StatusTooManyRequests, map[string]string{"error": "too many open streams"})
			return
		}
		defer streams.release(id.Subject)
		c, err := newSSE(rw)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = c.send("hello", map[string]int{"heartbeatSeconds": int(streamHeartbeat / time.Second)})
		st := &streamState{approvals: map[string][32]byte{}}
		// start after the newest event: the client loads the list itself, the stream sends what is new
		if w.events != nil {
			if l, err := w.events.List(ctx, eventFilter{Limit: 1}); err == nil && len(l) > 0 {
				st.lastEvent = l[0].ID
			}
		}
		may, permAt := w.writeAccess(ctx, id), time.Now()
		end := time.NewTimer(streamMaxAge)
		defer end.Stop()
		poll := time.NewTicker(streamPoll)
		defer poll.Stop()
		beat := time.NewTicker(streamHeartbeat)
		defer beat.Stop()
		for first := true; ; first = false {
			if !first {
				select {
				case <-ctx.Done():
					return
				case <-end.C:
					_ = c.send("bye", map[string]string{"reason": "max-age"})
					return
				case <-beat.C:
					if c.send("heartbeat", nil) != nil {
						return
					}
					continue
				case <-poll.C:
				}
			}
			if time.Since(permAt) > streamPermTTL {
				may, permAt = w.writeAccess(ctx, id), time.Now()
			}
			if err := w.streamPass(ctx, c, st, id, may); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("stream %s: %v", id.Subject, err)
				}
				return
			}
		}
	})
}

// streamPass sends what changed since the last pass. An error ends the stream (the client is gone).
func (w *writer) streamPass(ctx context.Context, c *sseConn, st *streamState, id *identity, may func(forge, repo string) bool) error {
	admin := w.isAdmin(id)
	canSee := func(forge, repo string) bool { return admin || may(forge, repo) }
	var visible []agent
	names := map[string]bool{}
	if lister, ok := w.store.(agentLister); ok {
		if list, err := lister.List(ctx); err == nil {
			for _, a := range list {
				for _, repo := range a.Repos {
					if canSee(a.Forge, repo) {
						visible = append(visible, a)
						names[a.Name] = true
						break
					}
				}
				if admin && len(a.Repos) == 0 {
					visible = append(visible, a)
					names[a.Name] = true
				}
			}
			if visible == nil {
				visible = []agent{}
			}
			if h := hashJSON(visible); h != st.agents {
				st.agents = h
				if err := c.send("agents", visible); err != nil {
					return err
				}
			}
		}
	}
	if as, ok := w.store.(approvalStore); ok {
		if objs, err := as.ListApprovals(ctx); err == nil {
			seen := map[string]bool{}
			for _, u := range objs {
				a := fromApprovalObject(u)
				if !canSee(a.Forge, a.Repo) {
					continue
				}
				seen[a.Name] = true
				if h := hashJSON(a); h != st.approvals[a.Name] {
					st.approvals[a.Name] = h
					if err := c.send("approval", a); err != nil {
						return err
					}
				}
			}
			for name := range st.approvals {
				if !seen[name] {
					delete(st.approvals, name)
					if err := c.send("approval-removed", map[string]string{"name": name}); err != nil {
						return err
					}
				}
			}
		}
	}
	if w.events != nil {
		f := eventFilter{Limit: 100}
		if !admin {
			f.Agents = make([]string, 0, len(names))
			for n := range names {
				f.Agents = append(f.Agents, n)
			}
		}
		if l, err := w.events.List(ctx, f); err == nil {
			for i := len(l) - 1; i >= 0; i-- { // oldest first
				if l[i].ID > st.lastEvent {
					if err := c.send("security-event", l[i]); err != nil {
						return err
					}
				}
			}
			for _, e := range l {
				st.lastEvent = max(st.lastEvent, e.ID)
			}
		}
	}
	if w.usage != nil {
		s, _, err1 := w.usage.Settings(ctx)
		u, err2 := w.usage.State(ctx)
		if err1 == nil && err2 == nil {
			h := hashJSON([]any{s, u})
			if st.usageSeen && h != st.usage {
				if err := c.send("usage", nil); err != nil {
					return err
				}
			}
			st.usage, st.usageSeen = h, true
		}
	}
	return ctx.Err()
}
