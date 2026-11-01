package main

// Per-agent hash chain (#71). Each stored event carries prev, the chain value of the agent's previous event, and
// chain = SHA-256(prev "\n" canonical event). A SIEM that keeps the last chain value per agent detects a missing,
// edited, reordered or inserted event: the next event's prev no longer matches, or its chain no longer recomputes.
// The chain is computed by the API when it inserts the row, under a per-agent lock, so the order is the id order.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// chainFields is the canonical form that the chain covers: every schema field except id (assigned by the database
// after the chain is computed) and the chain fields themselves. The field order is fixed by this struct; json.Marshal
// writes it in that order without extra spaces, so the bytes are stable.
type chainFields struct {
	Schema    string `json:"schema"`
	Time      string `json:"time"`
	Agent     string `json:"agent"`
	Job       string `json:"job"`
	Source    string `json:"source"`
	Untrusted bool   `json:"untrusted"`
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	Action    string `json:"action"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Hash      string `json:"hash"`
}

// canonical returns the bytes the chain covers. time is RFC 3339 with nanoseconds in UTC, as stored.
func canonical(e securityEvent) []byte {
	b, _ := json.Marshal(chainFields{Schema: e.Schema, Time: e.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Agent: e.Agent, Job: e.Job, Source: e.Source, Untrusted: e.Untrusted, Rule: e.Rule, Severity: e.Severity,
		Action: e.Action, Pod: e.Pod, Container: e.Container, Hash: e.Hash})
	return b
}

// chainHash links e to prev (the empty string starts a chain).
func chainHash(prev string, e securityEvent) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte("\n"))
	h.Write(canonical(e))
	return hex.EncodeToString(h.Sum(nil))
}

// link sets e.Prev and e.Chain after the agent's last chain value.
func link(prev string, e *securityEvent) {
	e.Prev = prev
	e.Chain = chainHash(prev, *e)
}

// verifyChain checks events of one agent in id order (oldest first), starting after the chain value start ("" for
// the first event ever). It returns the index of the first bad event and why, or -1.
func verifyChain(start string, events []securityEvent) (int, error) {
	prev := start
	for i, e := range events {
		if e.Prev != prev {
			return i, fmt.Errorf("event %d: prev %.12s does not match the previous chain %.12s (gap or reorder)", e.ID, e.Prev, prev)
		}
		if want := chainHash(prev, e); e.Chain != want {
			return i, fmt.Errorf("event %d: chain does not recompute (edited)", e.ID)
		}
		prev = e.Chain
	}
	return -1, nil
}
