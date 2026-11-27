package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// POST /run-events (localhost API, #425): the run log of a job. The loop reads Claude's stream-json while it runs and
// sends batches of run events (assistant text, tool calls, tool results, the final result), already redacted and
// clipped. The sidecar is the structured channel, as for /usage: it checks the schema and the sizes, redacts again
// with its own token and forwards the batch to the API's internal listener (APPROVALS_URL, the path the sidecar
// already uses for approval decisions; NetworkPolicy: agent pods only). The API binds the batch to this agent by the
// pod address (images/api/runlog.go), so no new credential is needed and no other agent's job can be written.
//
// Run events are model output: they are never printed to the container log, never posted on the forge and never
// part of a security event. When the API is away, the batch is dropped (the run log is best effort; the job goes on).
//
// The body is {"repo": "owner/name" (optional with one repo), "job": "issue-N" | "pr-N", "events": [...]}. The job
// names the unit the events belong to; another kind of unit (a console chat session, #424) can use the same endpoint
// with its own job pattern later.

const (
	runEventsMaxBody    = 256 << 10 // one batch
	runEventsMaxEvents  = 100
	runEventMaxText     = 4096 // the loop clips at 2 KB; the margin is for the clip marker and escaping
	runEventsPerMinute  = 240
	runEventsPostTimout = 5 * time.Second
)

var runEventKinds = map[string]bool{"text": true, "tool_use": true, "tool_result": true, "result": true, "truncated": true}

type runEvent struct {
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

type runEventBatch struct {
	Repo   string     `json:"repo,omitempty"`
	Job    string     `json:"job"`
	Events []runEvent `json:"events"`
}

type runEventGate struct {
	mu   sync.Mutex
	seen []time.Time
}

func (g *runEventGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	recent := g.seen[:0]
	for _, t := range g.seen {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	g.seen = recent
	if len(g.seen) >= runEventsPerMinute {
		return false
	}
	g.seen = append(g.seen, now)
	return true
}

// runBearerRe: a bare bearer token (an Authorization header in a command or an output); runKeyTailRe: a private key
// whose END line was cut off (a clipped output), redacted to the end of the text.
var (
	runBearerRe  = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',}]{12,}`)
	runKeyTailRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*$`)
)

// redactText: the credential shapes (secretRe), bare bearer tokens, an unterminated private key and the sidecar's
// forge token, as for approval requests.
func (s *server) redactText(v string) string {
	if t := s.c.Token; len(t) >= 8 {
		v = strings.ReplaceAll(v, t, "[redacted]")
	}
	v = secretRe.ReplaceAllStringFunc(v, func(m string) string {
		if sm := secretRe.FindStringSubmatch(m); sm[2] != "" {
			return sm[2] + "[redacted]"
		}
		return "[redacted]"
	})
	v = runBearerRe.ReplaceAllString(v, "${1}[redacted]")
	return runKeyTailRe.ReplaceAllString(v, "[redacted]")
}

func clipUTF8(v string, n int) string {
	if len(v) <= n {
		return v
	}
	v = v[:n]
	for !utf8.ValidString(v) && len(v) > 0 {
		v = v[:len(v)-1]
	}
	return v + " [clipped]"
}

// checkRunEvents validates a batch and returns it cleaned (redacted, clipped); an error is a refusal.
func (s *server) checkRunEvents(b *runEventBatch) error {
	if !jobRe.MatchString(b.Job) {
		return fmt.Errorf("job must be issue-N or pr-N")
	}
	if b.Repo == "" && len(s.c.Repos) == 1 {
		b.Repo = s.c.Repos[0]
	}
	if !s.repoAllowed(b.Repo) {
		return fmt.Errorf("repo is not one of this agent's")
	}
	if len(b.Events) == 0 || len(b.Events) > runEventsMaxEvents {
		return fmt.Errorf("1..%d events", runEventsMaxEvents)
	}
	for i := range b.Events {
		e := &b.Events[i]
		if !runEventKinds[e.Kind] {
			return fmt.Errorf("event %d: unknown kind", i)
		}
		if t, err := time.Parse(time.RFC3339, e.Time); err != nil || t.After(time.Now().Add(time.Hour)) {
			e.Time = time.Now().UTC().Format(time.RFC3339)
		}
		if e.Size < 0 || e.Turns < 0 || e.InputTokens < 0 || e.OutputTokens < 0 ||
			e.Size > 1<<40 || e.Turns > 1e6 || e.InputTokens > 1e10 || e.OutputTokens > 1e10 {
			return fmt.Errorf("event %d: number out of range", i)
		}
		e.Text = clipUTF8(s.redactText(e.Text), runEventMaxText)
		e.Input = clipUTF8(s.redactText(e.Input), runEventMaxText)
		e.Tool = clipUTF8(e.Tool, 128)
		e.Subtype = clipUTF8(e.Subtype, 64)
	}
	return nil
}

// runEventRepoRe: a repo value that can be shown in a security event as it is (owner/name, no other characters).
var runEventRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// refusedBatch: what an administrator needs to find the sender of a refused batch (#577): the repo it named (only when
// it has the owner/name form; else a fixed text), the number of events and a short hash of the first event. Never
// the event text: run events are model output.
func refusedBatch(b *runEventBatch) string {
	repo := "repo " + b.Repo
	switch {
	case b.Repo == "":
		repo = "no repo"
	case !runEventRepoRe.MatchString(b.Repo):
		repo = "repo not of the form owner/name"
	}
	first := "none"
	if len(b.Events) > 0 {
		j, _ := json.Marshal(b.Events[0])
		h := sha256.Sum256(j)
		first = hex.EncodeToString(h[:6])
	}
	return fmt.Sprintf("%s, %d events, first event sha256 %s", repo, len(b.Events), first)
}

func (s *server) runEvents(w http.ResponseWriter, r *http.Request) {
	var b runEventBatch
	dec := json.NewDecoder(io.LimitReader(r.Body, runEventsMaxBody+1))
	dec.DisallowUnknownFields()
	if r.ContentLength > runEventsMaxBody {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := dec.Decode(&b); err != nil {
		s.event(r, "run-events-invalid", "low", "run events are not valid JSON of the expected schema")
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := s.checkRunEvents(&b); err != nil {
		s.event(r, "run-events-invalid", "low", "run events refused: "+err.Error()+" ("+refusedBatch(&b)+")")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.runGate.allow(time.Now()) {
		http.Error(w, "too many batches", http.StatusTooManyRequests)
		return
	}
	if s.c.APIURL == "" {
		w.WriteHeader(http.StatusNoContent) // no API: dropped
		return
	}
	body, _ := json.Marshal(b) // re-encoded: only the known fields go on
	ctx, cancel := context.WithTimeout(r.Context(), runEventsPostTimout)
	defer cancel()
	u := fmt.Sprintf("%s/internal/run-events/%s", strings.TrimRight(s.c.APIURL, "/"), url.PathEscape(s.c.AgentName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "api", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "api unavailable", http.StatusBadGateway) // the loop drops the batch
		return
	}
	resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
}
