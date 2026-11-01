package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// POST /usage (localhost API, #93, review #102): the loop's usage report after each Claude run. The sidecar is the
// structured channel for usage: it checks the schema and the ranges, rate-limits the reports and prints the only
// `AGENTDEV_USAGE {json}` line the operator accepts, on the forge container's stdout. The agent container's log
// (where Claude's output and anything a test script prints end up) is never read for usage.
//
// The model runs in the same pod and can reach 127.0.0.1:7070 from a tool it may run (a test script), so a report is
// not proof that Claude counted it. That residual is limited by the operator (a report can only raise a pause, never
// lift or shorten one, a pause from one report is capped) and recorded in docs/security.md (R47). The full fix moves
// usage observation into the credential proxy (#66), outside the model's reach.

const usagePrefix = "AGENTDEV_USAGE "

// usageMaxPerMinute: at most this many reports per minute are printed (a few jobs run at once); more are refused and
// raise one security event per burst.
var usageMaxPerMinute = 6

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

type usageReport struct {
	Job          string       `json:"job"`
	InputTokens  int64        `json:"input_tokens"`
	OutputTokens int64        `json:"output_tokens"`
	RateLimited  bool         `json:"rate_limited"`
	ResetsAt     int64        `json:"resets_at"`
	FiveHour     *usageWindow `json:"five_hour"`
	Weekly       *usageWindow `json:"seven_day"`
}

type usageGate struct {
	mu   sync.Mutex
	seen []time.Time // accepted reports in the last minute
	warn bool
}

var usageOut io.Writer = eventOut

func validWindow(w *usageWindow, now time.Time, length time.Duration) bool {
	return w == nil || (!math.IsNaN(w.Utilization) && !math.IsInf(w.Utilization, 0) && w.Utilization >= 0 &&
		w.Utilization <= 10 && w.ResetsAt >= 0 && w.ResetsAt <= now.Add(length+time.Hour).Unix())
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	var u usageReport
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		s.event(r, "usage-report-invalid", "medium", "usage report is not valid JSON of the expected schema")
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if !jobRe.MatchString(u.Job) {
		u.Job = ""
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.InputTokens > 1e10 || u.OutputTokens > 1e10 ||
		u.ResetsAt < 0 || u.ResetsAt > now.Add(8*24*time.Hour).Unix() ||
		!validWindow(u.FiveHour, now, 5*time.Hour) || !validWindow(u.Weekly, now, 7*24*time.Hour) {
		s.event(r, "usage-report-invalid", "medium", fmt.Sprintf("usage report out of range (resets_at %d)", u.ResetsAt))
		http.Error(w, "out of range", http.StatusBadRequest)
		return
	}
	g := &s.usageGate
	g.mu.Lock()
	recent := g.seen[:0]
	for _, t := range g.seen {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	g.seen = recent
	if len(g.seen) >= usageMaxPerMinute {
		first := !g.warn
		g.warn = true
		g.mu.Unlock()
		if first {
			s.event(r, "usage-report-flood", "medium", fmt.Sprintf("more than %d usage reports in a minute", usageMaxPerMinute))
		}
		http.Error(w, "too many reports", http.StatusTooManyRequests)
		return
	}
	g.seen, g.warn = append(g.seen, now), false
	g.mu.Unlock()
	b, _ := json.Marshal(u) // re-encoded: only the known fields reach the log
	eventMu.Lock()
	fmt.Fprintf(usageOut, "%s%s\n", usagePrefix, b)
	eventMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
