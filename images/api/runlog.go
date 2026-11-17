package main

// Run log of agent jobs (#425): what the agent does while a job runs. The worker reads Claude's stream-json line by
// line and sends run events (assistant text, tool calls and results, the final result; redacted and clipped) through
// its forge sidecar to the internal listener: POST /internal/run-events/{agent}. The console reads them with
// GET /api/jobs/{agent}/{job}/events (paged, after=<id>) and follows a running job with
// GET /api/jobs/{agent}/{job}/events/stream (SSE, the envelope of stream.go).
//
// Trust: run events are model output (untrusted). They are stored as data, served as JSON and shown as plain text;
// they never reach the forge, notifications, security events or a log line. The ingest binds a batch to the agent by
// the caller's pod address: only a running pod app=coding-agent,agent=<agent> (the operator's labels) may write that
// agent's events, so an agent cannot write the run log of another agent's job. The repository must be one of the
// agent's. Inside its own pod the model could reach the sidecar and write its own run log; that is what the log shows
// anyway (its own actions), and it is marked as unchecked in the console.
//
// Visibility (R35): the same rule as GET /api/jobs: admins, and people with forge write access on the repository.
// Storage: Postgres table job_events (append-only, 30 days, job_events_prune()), or memory without a database.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	runEventsMaxBody   = 256 << 10
	runEventsMaxBatch  = 100
	runEventMaxData    = 12 << 10
	runLogMaxPerJob    = 3 << 20 // the worker stops at 2 MB; the API refuses past 3 MB
	runLogRetention    = 30 * 24 * time.Hour
	runStreamPoll      = time.Second
	runStreamHeartbeat = 15 * time.Second
	runStreamMaxAge    = 30 * time.Minute
)

var (
	runJobRe     = regexp.MustCompile(`^(issue|pr)-[0-9]{1,10}$`)
	runEventKind = map[string]bool{"text": true, "tool_use": true, "tool_result": true, "result": true, "truncated": true}
)

// jobEvent is one stored run event. Data holds the event's fields (text, tool, input, ok, size, turns, tokens).
type jobEvent struct {
	ID    int64           `json:"id"`
	Agent string          `json:"agent"`
	Repo  string          `json:"repo"`
	Job   string          `json:"job"`
	Time  time.Time       `json:"time"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data"`
}

type jobEventStore interface {
	AppendJobEvents(ctx context.Context, evs []jobEvent) error
	JobEvents(ctx context.Context, agent, repo, job string, after int64, limit int) ([]jobEvent, error)
	JobLogBytes(ctx context.Context, agent, repo, job string) (int64, error)
	PruneJobEvents(ctx context.Context) (int64, error)
}

// ---- memory ----

type memJobEvents struct {
	mu     sync.Mutex
	next   int64
	rows   []jobEvent
	stored []time.Time
}

func (m *memJobEvents) AppendJobEvents(_ context.Context, evs []jobEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range evs {
		m.next++
		e.ID = m.next
		m.rows = append(m.rows, e)
		m.stored = append(m.stored, now())
	}
	return nil
}

func (m *memJobEvents) JobEvents(_ context.Context, agent, repo, job string, after int64, limit int) ([]jobEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []jobEvent{}
	for _, e := range m.rows {
		if e.Agent == agent && e.Repo == repo && e.Job == job && e.ID > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memJobEvents) JobLogBytes(_ context.Context, agent, repo, job string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, e := range m.rows {
		if e.Agent == agent && e.Repo == repo && e.Job == job {
			n += int64(len(e.Data))
		}
	}
	return n, nil
}

func (m *memJobEvents) PruneJobEvents(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := now().Add(-runLogRetention)
	var n int64
	rows, stored := m.rows[:0], m.stored[:0]
	for i, e := range m.rows {
		if m.stored[i].Before(cut) {
			n++
			continue
		}
		rows, stored = append(rows, e), append(stored, m.stored[i])
	}
	m.rows, m.stored = rows, stored
	return n, nil
}

// ---- Postgres ----

func (s *pgStore) AppendJobEvents(ctx context.Context, evs []jobEvent) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, e := range evs {
		if _, err := tx.Exec(ctx, `INSERT INTO job_events (agent, repo, job, time, kind, data) VALUES ($1,$2,$3,$4,$5,$6)`,
			e.Agent, e.Repo, e.Job, e.Time, e.Kind, []byte(e.Data)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *pgStore) JobEvents(ctx context.Context, agent, repo, job string, after int64, limit int) ([]jobEvent, error) {
	rows, err := s.db.Query(ctx, `SELECT id, agent, repo, job, time, kind, data FROM job_events
		WHERE agent = $1 AND repo = $2 AND job = $3 AND id > $4 ORDER BY id LIMIT $5`, agent, repo, job, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobEvent{}
	for rows.Next() {
		var e jobEvent
		var b []byte
		if err := rows.Scan(&e.ID, &e.Agent, &e.Repo, &e.Job, &e.Time, &e.Kind, &b); err != nil {
			return nil, err
		}
		e.Data = b
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) JobLogBytes(ctx context.Context, agent, repo, job string) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT coalesce(sum(octet_length(data::text)), 0) FROM job_events
		WHERE agent = $1 AND repo = $2 AND job = $3`, agent, repo, job).Scan(&n)
	return n, err
}

func (s *pgStore) PruneJobEvents(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT job_events_prune()`).Scan(&n)
	return n, err
}

// ---- ingest (internal listener) ----

// agentPodIPs gives the addresses of the agent's running pods (operator labels app=coding-agent,agent=<name>).
type agentPodIPs interface {
	AgentPodIPs(ctx context.Context, agent string) ([]string, error)
}

func (k *kubeAgents) AgentPodIPs(ctx context.Context, agent string) ([]string, error) {
	l, err := k.c.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent,agent=" + agent})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range l.Items {
		if p.Status.Phase != "Running" || p.DeletionTimestamp != nil {
			continue
		}
		for _, ip := range p.Status.PodIPs {
			out = append(out, ip.IP)
		}
		if p.Status.PodIP != "" {
			out = append(out, p.Status.PodIP)
		}
	}
	return out, nil
}

type runEventIn struct {
	Kind         string `json:"kind"`
	Time         string `json:"time"`
	Text         string `json:"text,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Input        string `json:"input,omitempty"`
	OK           *bool  `json:"ok,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Subtype      string `json:"subtype,omitempty"`
	Turns        int64  `json:"turns,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
}

type runLog struct {
	store  jobEventStore
	agents agentLister
	pods   agentPodIPs
}

// ingest: POST /internal/run-events/{agent} {"repo", "job", "events": [...]}, from the agent's forge sidecar.
func (rl *runLog) ingest() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentName := r.PathValue("agent")
		if !agentNameRe.MatchString(agentName) || len(agentName) > 63 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad agent"})
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		ips, err := rl.pods.AgentPodIPs(r.Context(), agentName)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cannot check the caller"})
			return
		}
		if !slices.Contains(ips, host) {
			// another agent's pod, or anything else in the agents namespace: refused, nothing stored
			log.Printf("run events for %s refused: caller is not a pod of that agent", agentName)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var in struct {
			Repo   string       `json:"repo"`
			Job    string       `json:"job"`
			Events []runEventIn `json:"events"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, runEventsMaxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || !runJobRe.MatchString(in.Job) || !validRepo(in.Repo) ||
			len(in.Events) == 0 || len(in.Events) > runEventsMaxBatch {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad batch"})
			return
		}
		a, ok := findAgent(r.Context(), rl.agents, agentName)
		if !ok || !slices.Contains(a.Repos, in.Repo) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a repository of this agent"})
			return
		}
		used, err := rl.store.JobLogBytes(r.Context(), agentName, in.Repo, in.Job)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store"})
			return
		}
		evs := make([]jobEvent, 0, len(in.Events))
		for _, e := range in.Events {
			if !runEventKind[e.Kind] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad event kind"})
				return
			}
			t, err := time.Parse(time.RFC3339, e.Time)
			if err != nil || t.After(now().Add(time.Hour)) || t.Before(now().Add(-runLogRetention)) {
				t = now()
			}
			kind := e.Kind
			e.Kind, e.Time = "", ""
			b, _ := json.Marshal(e)
			if len(b) > runEventMaxData {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "event too large"})
				return
			}
			used += int64(len(b))
			if used > runLogMaxPerJob {
				break // over the cap: the rest of the job's log is dropped
			}
			evs = append(evs, jobEvent{Agent: agentName, Repo: in.Repo, Job: in.Job, Time: t.UTC(), Kind: kind, Data: b})
		}
		if len(evs) > 0 {
			if err := rl.store.AppendJobEvents(r.Context(), evs); err != nil {
				log.Printf("run events for %s: store: %v", agentName, err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store"})
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func findAgent(ctx context.Context, l agentLister, name string) (agent, bool) {
	if l == nil {
		return agent{}, false
	}
	list, err := l.List(ctx)
	if err != nil {
		return agent{}, false
	}
	for _, a := range list {
		if a.Name == name {
			return a, true
		}
	}
	return agent{}, false
}

// ---- console ----

// jobOf checks the path and the caller's access (R35, as GET /api/jobs). 404 when the caller may not see the job.
func (w *writer) runJobOf(rw http.ResponseWriter, r *http.Request) (agent, string, string, bool) {
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	name, jobName := r.PathValue("agent"), r.PathValue("job")
	if !agentNameRe.MatchString(name) || len(name) > 63 || !runJobRe.MatchString(jobName) {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "bad agent or job"})
		return agent{}, "", "", false
	}
	repo := r.URL.Query().Get("repo")
	if repo != "" && !validRepo(repo) {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "repo must be owner/name"})
		return agent{}, "", "", false
	}
	l, _ := w.store.(agentLister)
	a, ok := findAgent(r.Context(), l, name)
	if ok && repo == "" && len(a.Repos) == 1 {
		repo = a.Repos[0]
	}
	if !ok || repo == "" || !slices.Contains(a.Repos, repo) || id == nil ||
		!(w.isAdmin(id) || w.writeAccess(r.Context(), id)(a.Forge, repo)) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"}) // do not reveal other projects
		return agent{}, "", "", false
	}
	return a, repo, jobName, true
}

func parseAfter(r *http.Request) (int64, bool) {
	v := r.URL.Query().Get("after")
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0 && digitsAnyRe.MatchString(v)
}

// listRunEvents: GET /api/jobs/{agent}/{job}/events?repo=&after=&limit= (oldest first; next_after when more).
func (w *writer) listRunEvents(rw http.ResponseWriter, r *http.Request) {
	if w.runlog == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	a, repo, jobName, ok := w.runJobOf(rw, r)
	if !ok {
		return
	}
	after, ok := parseAfter(r)
	if !ok {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "after must be an event id"})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "limit must be 1..500"})
			return
		}
		limit = n
	}
	evs, err := w.runlog.JobEvents(r.Context(), a.Name, repo, jobName, after, limit)
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read the run log"})
		return
	}
	resp := map[string]any{"events": evs}
	if len(evs) == limit {
		resp["next_after"] = evs[len(evs)-1].ID
	}
	writeJSON(rw, http.StatusOK, resp)
}

// streamRunEvents: GET /api/jobs/{agent}/{job}/events/stream?repo=&after=, server-sent events "run-event" (one
// stored event each), with hello, heartbeat and bye as in stream.go. Access is checked again every 5 minutes.
func (w *writer) streamRunEvents(rw http.ResponseWriter, r *http.Request) {
	if w.runlog == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	a, repo, jobName, ok := w.runJobOf(rw, r)
	if !ok {
		return
	}
	after, ok := parseAfter(r)
	if !ok {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "after must be an event id"})
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
	if c.send("hello", map[string]int{"heartbeatSeconds": int(runStreamHeartbeat / time.Second)}) != nil {
		return
	}
	end := time.Now().Add(runStreamMaxAge)
	permAt, beat := time.Now(), time.Now()
	tick := time.NewTicker(runStreamPoll)
	defer tick.Stop()
	for {
		evs, err := w.runlog.JobEvents(ctx, a.Name, repo, jobName, after, 200)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				_ = c.send("bye", map[string]string{"reason": "cannot read the run log"})
			}
			return
		}
		for _, e := range evs {
			if c.send("run-event", e) != nil {
				return
			}
			after = e.ID
		}
		if len(evs) > 0 {
			beat = time.Now()
		}
		if time.Since(beat) >= runStreamHeartbeat {
			if c.send("heartbeat", nil) != nil {
				return
			}
			beat = time.Now()
		}
		if time.Now().After(end) {
			_ = c.send("bye", map[string]string{"reason": "max-age"})
			return
		}
		if time.Since(permAt) > streamPermTTL {
			if !(w.isAdmin(id) || w.writeAccess(ctx, id)(a.Forge, repo)) {
				_ = c.send("bye", map[string]string{"reason": "access"})
				return
			}
			permAt = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pruneJobEvents removes run events older than 30 days, once a day.
func pruneJobEvents(ctx context.Context, s jobEventStore) {
	for {
		if _, err := s.PruneJobEvents(ctx); err != nil {
			log.Printf("prune job_events: %v", err)
		}
		if !sleepCtx(ctx, 24*time.Hour) {
			return
		}
	}
}
