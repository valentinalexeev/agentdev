package main

// Jobs (#345): GET /api/jobs for the console's Work board (docs/console-ux.md §7, §11).
//
// Source: the forge, read with the API's own read-only credential. A job is one issue handed to an agent and the PR
// the agent opened for it. Why the forge and not botboard or the worker: the forge is the record people and agents
// already trust (the branch prefix agent/<name>/ is enforced by the forge sidecar, images/forge/git.go, so a PR on it
// is the agent's; the hand-over is an assignment on Gitea or the label agent:<name> on GitHub, both set by people with
// write access). botboard is optional, has no authentication on its write API, and its cards carry text from the loop;
// the worker's state lives in the agent pod, which the model can write. Neither may feed the console.
//
// Phases: queued (issue handed over, no PR yet), build (the same, with a pending approval for that job: the agent is
// working and waits), review (PR open), done (PR merged or closed). Outcome for done: merged | closed.
// The Approval objects (operator-written, trusted) add the build phase; nothing comes from agent output.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type job struct {
	ID      string    `json:"id"` // <agent>/<repo>/issue-<n> or <agent>/<repo>/pr-<n> (a PR without an issue number in its branch)
	Agent   string    `json:"agent"`
	Repo    string    `json:"repo"`
	Forge   string    `json:"forge"`
	Issue   int       `json:"issue,omitempty"`
	PR      int       `json:"pr,omitempty"`
	Title   string    `json:"title"` // untrusted: written by people or the agent on the forge; render as text
	URL     string    `json:"url,omitempty"`
	PRURL   string    `json:"prUrl,omitempty"`
	Phase   string    `json:"phase"`             // queued | build | review | done
	Outcome string    `json:"outcome,omitempty"` // merged | closed (done only)
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Closed  time.Time `json:"closed,omitzero"`
}

type forgePR struct {
	Number  int
	Title   string
	URL     string
	Branch  string
	State   string // open | closed
	Merged  bool
	Created time.Time
	Updated time.Time
	Closed  time.Time
}

type forgeIssue struct {
	Number  int
	Title   string
	URL     string
	Created time.Time
	Updated time.Time
}

// jobForge lists the PRs of a repository and the open issues handed to an agent.
type jobForge interface {
	Pulls(ctx context.Context, forge, repo string) ([]forgePR, error)
	AgentIssues(ctx context.Context, forge, repo, agent string) ([]forgeIssue, error)
}

var branchIssueRe = regexp.MustCompile(`^agent/([a-z0-9]([-a-z0-9]*[a-z0-9])?)/issue-([0-9]{1,9})$`)

// buildJobs joins the PRs and issues of one agent on one repository. pending: job names (issue-N) with a pending approval.
func buildJobs(a agent, repo string, prs []forgePR, issues []forgeIssue, pending map[string]bool) []job {
	out := []job{}
	byIssue := map[int]bool{}
	prefix := "agent/" + a.Name + "/"
	for _, p := range prs {
		if !strings.HasPrefix(p.Branch, prefix) {
			continue
		}
		j := job{Agent: a.Name, Repo: repo, Forge: a.Forge, PR: p.Number, Title: p.Title, PRURL: p.URL,
			Created: p.Created, Updated: p.Updated, Closed: p.Closed, Phase: "review"}
		if m := branchIssueRe.FindStringSubmatch(p.Branch); m != nil && m[1] == a.Name {
			j.Issue, _ = strconv.Atoi(m[3])
			byIssue[j.Issue] = true
			j.ID = fmt.Sprintf("%s/%s/issue-%d", a.Name, repo, j.Issue)
		} else {
			j.ID = fmt.Sprintf("%s/%s/pr-%d", a.Name, repo, p.Number)
		}
		if p.State == "closed" {
			j.Phase, j.Outcome = "done", "closed"
			if p.Merged {
				j.Outcome = "merged"
			}
		}
		out = append(out, j)
	}
	for _, is := range issues {
		if byIssue[is.Number] {
			continue
		}
		j := job{ID: fmt.Sprintf("%s/%s/issue-%d", a.Name, repo, is.Number), Agent: a.Name, Repo: repo, Forge: a.Forge,
			Issue: is.Number, Title: is.Title, URL: is.URL, Created: is.Created, Updated: is.Updated, Phase: "queued"}
		if pending[fmt.Sprintf("issue-%d", is.Number)] {
			j.Phase = "build"
		}
		out = append(out, j)
	}
	// issue links for PR jobs: same repository, the forge's web path
	for i := range out {
		if out[i].URL == "" && out[i].Issue > 0 && out[i].PRURL != "" {
			if k := strings.LastIndex(out[i].PRURL, "/pull"); k > 0 {
				out[i].URL = fmt.Sprintf("%s/issues/%d", out[i].PRURL[:k], out[i].Issue)
			}
		}
	}
	return out
}

// jobCache keeps forge answers for a short time, so a board with 24 projects does not call the forge on every load.
// #519: an answer older than ttl but younger than maxStale is served at once and refreshed in the background
// (stale-while-revalidate), so a page load waits for the forge only on a cold cache (a new API pod, or an entry
// older than maxStale). Forge calls run at most jobForgeParallel repositories at a time, for page loads and
// background refreshes together.
type jobCache struct {
	f        jobForge
	ttl      time.Duration // younger: served without a forge call
	maxStale time.Duration // younger: served, refreshed in the background; older: the request waits for the forge
	sem      chan struct{} // forge calls in flight, one slot per repository
	mu       sync.Mutex
	m        map[string]jobCacheEntry
	busy     map[string]bool // keys with a background refresh running
}

type jobCacheEntry struct {
	at     time.Time
	prs    []forgePR
	issues []forgeIssue
}

// jobForgeParallel: repositories read from the forge at the same time (2 calls each). GitHub allows at most 100
// concurrent requests per credential (secondary rate limit), Gitea has no concurrency limit by default.
const jobForgeParallel = 8

// jobRefreshTimeout bounds one background refresh (each forge call also has its own client timeout).
const jobRefreshTimeout = 30 * time.Second

func newJobCache(f jobForge, ttl, maxStale time.Duration) *jobCache {
	return &jobCache{f: f, ttl: ttl, maxStale: maxStale, sem: make(chan struct{}, jobForgeParallel),
		m: map[string]jobCacheEntry{}, busy: map[string]bool{}}
}

func (c *jobCache) get(ctx context.Context, forge, repo, agent string) ([]forgePR, []forgeIssue, error) {
	key := forge + "\x00" + repo + "\x00" + agent
	c.mu.Lock()
	e, ok := c.m[key]
	age := time.Since(e.at)
	if ok && age < c.ttl {
		c.mu.Unlock()
		return e.prs, e.issues, nil
	}
	if ok && age < c.maxStale {
		if !c.busy[key] {
			c.busy[key] = true
			go c.refresh(key, forge, repo, agent)
		}
		c.mu.Unlock()
		return e.prs, e.issues, nil
	}
	c.mu.Unlock()
	return c.fetch(ctx, key, forge, repo, agent)
}

// refresh reads one entry again in the background; on an error the stale entry stays until maxStale.
func (c *jobCache) refresh(key, forge, repo, agent string) {
	defer func() {
		c.mu.Lock()
		delete(c.busy, key)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), jobRefreshTimeout)
	defer cancel()
	if _, _, err := c.fetch(ctx, key, forge, repo, agent); err != nil {
		log.Printf("jobs: background refresh of %s on %s: %v", agent, repo, err)
	}
}

// fetch reads the PRs and the agent's issues of one repository (both calls at the same time) and stores them.
func (c *jobCache) fetch(ctx context.Context, key, forge, repo, agent string) ([]forgePR, []forgeIssue, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx) // a failed call stops its sibling
	defer cancel()
	var (
		prs             []forgePR
		issues          []forgeIssue
		prErr, issueErr error
		wg              sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if prs, prErr = c.f.Pulls(ctx, forge, repo); prErr != nil {
			cancel()
		}
	}()
	if issues, issueErr = c.f.AgentIssues(ctx, forge, repo, agent); issueErr != nil {
		cancel()
	}
	wg.Wait()
	if prErr != nil {
		return nil, nil, prErr
	}
	if issueErr != nil {
		return nil, nil, issueErr
	}
	c.mu.Lock()
	c.m[key] = jobCacheEntry{at: time.Now(), prs: prs, issues: issues}
	c.mu.Unlock()
	return prs, issues, nil
}

// pendingJobs: agent -> job names with a pending approval (operator-written Approval objects).
func (w *writer) pendingJobs(ctx context.Context) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	as, ok := w.store.(approvalStore)
	if !ok {
		return out
	}
	objs, err := as.ListApprovals(ctx)
	if err != nil {
		return out
	}
	for _, u := range objs {
		a := fromApprovalObject(u)
		if a.State == "pending" && a.Job != "" {
			if out[a.Agent] == nil {
				out[a.Agent] = map[string]bool{}
			}
			out[a.Agent][a.Job] = true
		}
	}
	return out
}

// jobsOf lists the jobs of the given agents on the repositories the filter keeps; forge errors skip that repository.
// The repositories are read at the same time (#519), bounded by the cache's jobForgeParallel.
func (w *writer) jobsOf(ctx context.Context, agents []agent, keepRepo func(forge, repo string) bool) ([]job, []string) {
	if w.jobs == nil {
		return []job{}, nil
	}
	pending := w.pendingJobs(ctx)
	type task struct {
		a    agent
		repo string
		js   []job
		err  error
	}
	var tasks []*task
	for _, a := range agents {
		for _, repo := range a.Repos {
			if keepRepo(a.Forge, repo) {
				tasks = append(tasks, &task{a: a, repo: repo})
			}
		}
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, jobForgeParallel) // goroutines per request; the cache bounds forge calls overall
	for _, t := range tasks {
		select {
		case limit <- struct{}{}:
		case <-ctx.Done():
			t.err = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer func() { <-limit; wg.Done() }()
			prs, issues, err := w.jobs.get(ctx, t.a.Forge, t.repo, t.a.Name)
			if err != nil {
				t.err = err
				return
			}
			t.js = buildJobs(t.a, t.repo, prs, issues, pending[t.a.Name])
		}()
	}
	wg.Wait()
	out := []job{}
	var failed []string
	for _, t := range tasks {
		if t.err != nil {
			log.Printf("jobs: %s on %s: %v", t.a.Name, t.repo, t.err)
			failed = append(failed, t.repo)
			continue
		}
		out = append(out, t.js...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID < out[j].ID
	})
	return out, failed
}

// recentJobs: the agent's newest jobs on the repositories the caller may see.
func (w *writer) recentJobs(ctx context.Context, id *identity, a agent) []job {
	sc := scope{may: w.writeAccess(ctx, id), admin: id != nil && w.isAdmin(id)}
	js, _ := w.jobsOf(ctx, []agent{a}, sc.visible)
	if len(js) > 10 {
		js = js[:10]
	}
	return js
}

// listJobs serves GET /api/jobs?agent=&repo=&phase=&scope=mine&limit=&cursor=.
func (w *writer) listJobs(rw http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	q := r.URL.Query()
	pg, err := parsePage(q, 100)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	phase := q.Get("phase")
	switch phase {
	case "", "queued", "build", "review", "done":
	default:
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "phase: queued, build, review or done"})
		return
	}
	sc, err := w.scopeOf(r.Context(), id, q)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	lister, ok := w.store.(agentLister)
	if !ok {
		writeJSON(rw, http.StatusOK, map[string]any{"jobs": []job{}})
		return
	}
	list, err := lister.List(r.Context())
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read agents"})
		return
	}
	var agents []agent
	for _, a := range list {
		if q.Get("agent") == "" || q.Get("agent") == a.Name {
			agents = append(agents, a)
		}
	}
	js, failed := w.jobsOf(r.Context(), agents, sc.visible) // R35: only repositories the caller may see
	filtered := js[:0]
	for _, j := range js {
		if phase == "" || j.Phase == phase {
			filtered = append(filtered, j)
		}
	}
	items, next := pg.slice(len(filtered))
	resp := map[string]any{"jobs": filtered[items[0]:items[1]]}
	if next != "" {
		resp["next_cursor"] = next
	}
	if len(failed) > 0 {
		resp["unavailable"] = failed // repositories the forge did not answer for (the board marks them stale)
	}
	writeJSON(rw, http.StatusOK, resp)
}

// ---- forge clients ----

// forgeJobs routes by forge kind, like forgePerms.
type forgeJobs struct {
	gitea  *giteaPerms
	github *githubForge
}

func (f forgeJobs) Pulls(ctx context.Context, forge, repo string) ([]forgePR, error) {
	if forge == "github" {
		if f.github == nil {
			return nil, fmt.Errorf("%w: no GitHub credential", errForgeUnavailable)
		}
		return f.github.Pulls(ctx, repo)
	}
	return f.gitea.Pulls(ctx, repo)
}

func (f forgeJobs) AgentIssues(ctx context.Context, forge, repo, agent string) ([]forgeIssue, error) {
	if forge == "github" {
		if f.github == nil {
			return nil, fmt.Errorf("%w: no GitHub credential", errForgeUnavailable)
		}
		return f.github.AgentIssues(ctx, repo, agent)
	}
	return f.gitea.AgentIssues(ctx, repo, agent)
}

type rawPR struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	URL      string     `json:"html_url"`
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Created  time.Time  `json:"created_at"`
	Updated  time.Time  `json:"updated_at"`
	Closed   *time.Time `json:"closed_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (p rawPR) pr() forgePR {
	out := forgePR{Number: p.Number, Title: p.Title, URL: p.URL, Branch: p.Head.Ref, State: p.State,
		Merged: p.Merged || p.MergedAt != nil, Created: p.Created, Updated: p.Updated}
	if p.Closed != nil {
		out.Closed = *p.Closed
	}
	return out
}

type rawIssue struct {
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	URL     string    `json:"html_url"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
	PR      *struct{} `json:"pull_request"`
}

func issuesOf(raw []rawIssue) []forgeIssue {
	out := []forgeIssue{}
	for _, i := range raw {
		if i.PR == nil {
			out = append(out, forgeIssue{Number: i.Number, Title: i.Title, URL: i.URL, Created: i.Created, Updated: i.Updated})
		}
	}
	return out
}

// Pulls: the 50 most recently updated PRs (open and closed).
func (g *giteaPerms) Pulls(ctx context.Context, repo string) ([]forgePR, error) {
	o, n, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("bad repo %q", repo)
	}
	var raw []rawPR
	if err := g.get(ctx, fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=all&sort=recentupdate&limit=50",
		url.PathEscape(o), url.PathEscape(n)), &raw); err != nil {
		return nil, err
	}
	out := make([]forgePR, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.pr())
	}
	return out, nil
}

// AgentIssues: open issues assigned to the agent's account (the agent loop's own rule, agent-loop.sh).
func (g *giteaPerms) AgentIssues(ctx context.Context, repo, agent string) ([]forgeIssue, error) {
	o, n, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("bad repo %q", repo)
	}
	var raw []rawIssue
	if err := g.get(ctx, fmt.Sprintf("/api/v1/repos/%s/%s/issues?state=open&type=issues&assigned_by=%s&limit=50",
		url.PathEscape(o), url.PathEscape(n), url.QueryEscape(agent)), &raw); err != nil {
		return nil, err
	}
	return issuesOf(raw), nil
}

func (g *githubForge) Pulls(ctx context.Context, repo string) ([]forgePR, error) {
	o, n, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var raw []rawPR
	if err := g.get(ctx, repo, fmt.Sprintf("/repos/%s/%s/pulls?state=all&sort=updated&direction=desc&per_page=50", o, n), &raw); err != nil {
		return nil, err
	}
	out := make([]forgePR, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.pr())
	}
	return out, nil
}

// AgentIssues on GitHub: open issues with the label agent:<name> (an App cannot be assigned).
func (g *githubForge) AgentIssues(ctx context.Context, repo, agent string) ([]forgeIssue, error) {
	o, n, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var raw []rawIssue
	if err := g.get(ctx, repo, fmt.Sprintf("/repos/%s/%s/issues?state=open&labels=%s&per_page=50", o, n,
		url.QueryEscape("agent:"+agent)), &raw); err != nil {
		return nil, err
	}
	return issuesOf(raw), nil
}
