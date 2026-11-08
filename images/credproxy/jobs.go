package main

import (
	"regexp"
	"sort"
	"sync"
	"time"
)

// Per-job attribution (#196, ADR 0002 D3). The worker sends X-Agentdev-Job: <job> with every model request. The
// header comes from the agent container, so the model can change it: it is used for reporting only (GET /status, the
// usage line), never for a limit that protects the instance. The proxy strips it before the request goes upstream.

var jobRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const (
	jobHeader    = "X-Agentdev-Job"
	jobRetention = 7 * 24 * time.Hour
	jobsPerAgent = 200 // oldest jobs are dropped first
)

// cleanJob returns the job id if it matches jobRe, else "" (dropped).
func cleanJob(v string) string {
	if jobRe.MatchString(v) {
		return v
	}
	return ""
}

type jobTotal struct {
	Agent    string           `json:"agent"`
	Job      string           `json:"job"`
	Units    int64            `json:"units"`
	USD      *float64         `json:"usd,omitempty"` // only when prices are configured (owner decision 2)
	Tokens   map[string]int64 `json:"tokens"`
	Requests int64            `json:"requests"`
	First    time.Time        `json:"first"`
	Last     time.Time        `json:"last"`
	Stopped  bool             `json:"stopped,omitempty"` // refused by CLAUDE_JOB_BUDGET_UNITS
	tokens   tokenCounts
}

type jobs struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]map[string]*jobTotal // agent -> job -> total
}

func newJobs() *jobs { return &jobs{now: time.Now, m: map[string]map[string]*jobTotal{}} }

func (j *jobs) add(agent, job string, units int64, t tokenCounts) {
	if job == "" {
		return
	}
	now := j.now()
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.m[agent]
	if a == nil {
		a = map[string]*jobTotal{}
		j.m[agent] = a
	}
	e := a[job]
	if e == nil {
		j.pruneLocked(agent, now)
		e = &jobTotal{Agent: agent, Job: job, First: now}
		a[job] = e
	}
	e.Units += units
	e.tokens.add(t)
	e.Requests++
	e.Last = now
}

// used returns the units a job spent so far (the loop guard of #196 D6 reads it).
func (j *jobs) used(agent, job string) int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.m[agent][job]; e != nil {
		return e.Units
	}
	return 0
}

// firstRefusal marks the job as stopped and is true only the first time (one event per job).
func (j *jobs) firstRefusal(agent, job string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	e := j.m[agent][job]
	if e == nil || e.Stopped {
		return false
	}
	e.Stopped = true
	return true
}

// pruneLocked drops jobs older than the retention and keeps room for one more job of the agent.
func (j *jobs) pruneLocked(agent string, now time.Time) {
	a := j.m[agent]
	for k, e := range a {
		if now.Sub(e.Last) > jobRetention {
			delete(a, k)
		}
	}
	for len(a) >= jobsPerAgent {
		var oldest string
		for k, e := range a {
			if oldest == "" || e.Last.Before(a[oldest].Last) {
				oldest = k
			}
		}
		delete(a, oldest)
	}
}

// snapshot lists all jobs of the last 7 days, most expensive first.
func (j *jobs) snapshot(p priceTable) []jobTotal {
	now := j.now()
	j.mu.Lock()
	out := []jobTotal{} // [] not null on GET /status
	for _, a := range j.m {
		for _, e := range a {
			if now.Sub(e.Last) > jobRetention {
				continue
			}
			c := *e
			c.Tokens = c.tokens.byName()
			if usd, ok := p.usdOf(c.Units); ok {
				c.USD = &usd
			}
			out = append(out, c)
		}
	}
	j.mu.Unlock()
	sort.Slice(out, func(a, b int) bool {
		if out[a].Units != out[b].Units {
			return out[a].Units > out[b].Units
		}
		return out[a].Job < out[b].Job
	})
	return out
}
