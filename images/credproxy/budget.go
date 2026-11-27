package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-agent budget (#66): cost units (#196, prices.go) in a rolling 24 h window, kept in hourly buckets. The
// limit is CLAUDE_BUDGET_UNITS for every agent (0 = no limit), or the file <budget dir>/<agent> (ConfigMap
// claude-proxy-budgets) for one agent. A request is refused before it is sent when the agent's spend in the window
// has reached its limit; a request that is running may finish, so the spend can go over the limit by one response.

const bucketCount = 24

type agentSpend struct {
	Hours  [bucketCount]int64 `json:"hours"`  // units per hour, index = unix hour % 24
	Stamps [bucketCount]int64 `json:"stamps"` // unix hour of each bucket
}

// stateFile is the persisted spend; Unit marks the counting unit.
type stateFile struct {
	Unit  string                 `json:"unit"`
	Spend map[string]*agentSpend `json:"spend"`
}

const stateUnit = "units"

type budget struct {
	mu       sync.Mutex
	spend    map[string]*agentSpend
	def      int64
	dir      string // per-agent limits (one file per agent); "" = none
	state    string // state file; "" = memory only
	now      func() time.Time
	exceeded map[string]bool // agent -> an exhausted event was already printed for this exhaustion
}

func newBudget(def int64, dir, state string) *budget {
	b := &budget{spend: map[string]*agentSpend{}, def: def, dir: dir, state: state, now: time.Now, exceeded: map[string]bool{}}
	if state != "" {
		if data, err := os.ReadFile(state); err == nil {
			// only state written in units (#196): a file from the token-counting proxy, or without the marker, is
			// dropped, so an in-place image change never reads token hours as units (review of #275)
			var f stateFile
			if json.Unmarshal(data, &f) == nil && f.Unit == stateUnit {
				b.spend = f.Spend
			}
			if b.spend == nil {
				b.spend = map[string]*agentSpend{}
			}
			for k, v := range b.spend {
				if !agentRe.MatchString(k) || v == nil {
					delete(b.spend, k)
				}
			}
		}
	}
	return b
}

// limit is the agent's limit in units per 24 h; 0 = no limit. A per-agent file must be a plain non-negative integer.
func (b *budget) limit(agent string) int64 {
	if b.dir != "" && agentRe.MatchString(agent) {
		if data, err := os.ReadFile(filepath.Join(b.dir, agent)); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && n >= 0 {
				return n
			}
		}
	}
	return b.def
}

func (b *budget) usedLocked(agent string, hour int64) int64 {
	s := b.spend[agent]
	if s == nil {
		return 0
	}
	var t int64
	for i := range s.Hours {
		if s.Stamps[i] > hour-bucketCount {
			t += s.Hours[i]
		}
	}
	return t
}

// allow reports whether the agent may send a request now, with its spend and limit.
func (b *budget) allow(agent string) (ok bool, used, limit int64) {
	limit = b.limit(agent)
	b.mu.Lock()
	defer b.mu.Unlock()
	used = b.usedLocked(agent, b.now().Unix()/3600)
	if limit == 0 || used < limit {
		b.exceeded[agent] = false
		return true, used, limit
	}
	return false, used, limit
}

// firstRefusal is true once per exhaustion, so one security event is printed, not one per request.
func (b *budget) firstRefusal(agent string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := !b.exceeded[agent]
	b.exceeded[agent] = true
	return first
}

// add records the units of a finished response.
func (b *budget) add(agent string, units int64) {
	if units <= 0 {
		return
	}
	hour := b.now().Unix() / 3600
	b.mu.Lock()
	s := b.spend[agent]
	if s == nil {
		s = &agentSpend{}
		b.spend[agent] = s
	}
	i := hour % bucketCount
	if s.Stamps[i] != hour {
		s.Stamps[i], s.Hours[i] = hour, 0
	}
	s.Hours[i] += units
	var data []byte
	if b.state != "" {
		data, _ = json.Marshal(stateFile{Unit: stateUnit, Spend: b.spend})
	}
	b.mu.Unlock()
	if data != nil {
		tmp := b.state + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			_ = os.Rename(tmp, b.state)
		}
	}
}

type budgetStatus struct {
	Agent   string   `json:"agent"`
	Used    int64    `json:"used"`  // cost units (#196)
	Limit   int64    `json:"limit"` // cost units; 0 = no limit
	UsedUSD *float64 `json:"usedUsd,omitempty"`
}

// snapshot lists every agent with spend in the window (for GET /status, read by the API for the UI).
func (b *budget) snapshot() []budgetStatus {
	b.mu.Lock()
	hour := b.now().Unix() / 3600
	agents := make([]string, 0, len(b.spend))
	used := map[string]int64{}
	for a := range b.spend {
		agents = append(agents, a)
		used[a] = b.usedLocked(a, hour)
	}
	b.mu.Unlock()
	out := make([]budgetStatus, 0, len(agents))
	for _, a := range agents {
		out = append(out, budgetStatus{Agent: a, Used: used[a], Limit: b.limit(a)})
	}
	return out
}
