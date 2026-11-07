package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// client is a minimal GitHub REST client (stdlib only). It reads, never writes.
type client struct {
	base  string // https://api.github.com
	repo  string // owner/name
	token string
	http  *http.Client
}

func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pages calls get for page 1..max until a short page; each page is decoded into a fresh slice and appended.
func pages[T any](ctx context.Context, c *client, path string, q url.Values, max int) ([]T, error) {
	var all []T
	for p := 1; p <= max; p++ {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		qq.Set("per_page", "100")
		qq.Set("page", strconv.Itoa(p))
		var page []T
		if err := c.get(ctx, path, qq, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 100 {
			break
		}
	}
	return all, nil
}

type ghUser struct {
	Login string `json:"login"`
}
type ghLabel struct {
	Name string `json:"name"`
}
type ghIssue struct {
	Number      int                     `json:"number"`
	Title       string                  `json:"title"`
	State       string                  `json:"state"`
	Assignees   []ghUser                `json:"assignees"`
	Labels      []ghLabel               `json:"labels"`
	Milestone   *struct{ Title string } `json:"milestone"`
	PullRequest *struct{}               `json:"pull_request"`
}
type ghPR struct {
	Number         int                       `json:"number"`
	Title          string                    `json:"title"`
	State          string                    `json:"state"`
	Draft          bool                      `json:"draft"`
	Merged         bool                      `json:"merged"`
	MergedAt       *string                   `json:"merged_at"`
	MergeableState string                    `json:"mergeable_state"`
	User           ghUser                    `json:"user"`
	Labels         []ghLabel                 `json:"labels"`
	Head           struct{ SHA, Ref string } `json:"head"`
	Base           struct{ Ref string }      `json:"base"`
}
type ghReview struct {
	ID       int64  `json:"id"`
	User     ghUser `json:"user"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}
type ghComment struct {
	ID       int64  `json:"id"`
	User     ghUser `json:"user"`
	Body     string `json:"body"`
	IssueURL string `json:"issue_url"`
	PRURL    string `json:"pull_request_url"`
	Path     string `json:"path"`
}

// fetch builds one Snapshot. With an empty cursor (baseline) it reads open items and the latest comments; with a
// cursor it reads everything updated since the cursor (minus an overlap, so clock skew loses nothing: duplicates
// are harmless because the reducer is keyed by id), plus every open PR, because checks change without moving
// the PR's updated_at.
func fetch(ctx context.Context, c *client, cursor string, now time.Time) (Snapshot, error) {
	snap := Snapshot{At: now.UTC().Format(time.RFC3339)}
	r := "/repos/" + c.repo

	since := ""
	if t, err := time.Parse(time.RFC3339, cursor); err == nil {
		since = t.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	}

	// issues and PRs as issues: open ones on a baseline, everything touched since the cursor otherwise
	q := url.Values{"state": {"open"}}
	if since != "" {
		q = url.Values{"state": {"all"}, "since": {since}, "sort": {"updated"}}
	}
	items, err := pages[ghIssue](ctx, c, r+"/issues", q, 5)
	if err != nil {
		return snap, err
	}
	prNums := map[int]bool{}
	for _, it := range items {
		if it.PullRequest != nil {
			prNums[it.Number] = true
			continue
		}
		oi := toObservedIssue(it)
		snap.Issues = append(snap.Issues, oi)
	}
	open, err := pages[ghPR](ctx, c, r+"/pulls", url.Values{"state": {"open"}}, 3)
	if err != nil {
		return snap, err
	}
	for _, p := range open {
		prNums[p.Number] = true
	}
	for n := range prNums {
		op, err := fetchPR(ctx, c, n)
		if err != nil {
			return snap, err
		}
		snap.PRs = append(snap.PRs, op)
	}

	// comments: conversation and inline
	cq := url.Values{"sort": {"created"}, "direction": {"desc"}}
	maxPages := 1
	if since != "" {
		cq = url.Values{"since": {since}, "sort": {"updated"}}
		maxPages = 5
	}
	ic, err := pages[ghComment](ctx, c, r+"/issues/comments", cq, maxPages)
	if err != nil {
		return snap, err
	}
	for _, x := range ic {
		snap.Comments = append(snap.Comments, Comment{ID: x.ID, Number: lastNum(x.IssueURL), User: x.User.Login, Body: x.Body})
	}
	pc, err := pages[ghComment](ctx, c, r+"/pulls/comments", cq, maxPages)
	if err != nil {
		return snap, err
	}
	for _, x := range pc {
		snap.Comments = append(snap.Comments, Comment{ID: x.ID, Number: lastNum(x.PRURL), User: x.User.Login, Inline: true, Path: x.Path, Body: x.Body})
	}

	// Decision state (#242): on every poll, every open needs-decision issue and its latest human and bot-marked
	// comments. Issues already in the list above are not added twice; the others are added so their state is fresh.
	nd, err := pages[ghIssue](ctx, c, r+"/issues", url.Values{"state": {"open"}, "labels": {"needs-decision"}}, 2)
	if err != nil {
		return snap, err
	}
	have := map[int]bool{}
	for _, i := range snap.Issues {
		have[i.Number] = true
	}
	for _, it := range nd {
		if it.PullRequest != nil {
			continue
		}
		if !have[it.Number] {
			snap.Issues = append(snap.Issues, toObservedIssue(it))
		}
		cs, err := pages[ghIssueComment](ctx, c, fmt.Sprintf("%s/issues/%d/comments", r, it.Number), nil, 5)
		if err != nil {
			return snap, err
		}
		h := LastHuman{Number: it.Number}
		for _, x := range cs { // oldest first
			if strings.HasPrefix(strings.TrimSpace(x.Body), botMarker) {
				h.BotAt = x.CreatedAt
			} else {
				h.User, h.HumanAt = x.User.Login, x.CreatedAt
			}
		}
		snap.LastHuman = append(snap.LastHuman, h)
	}

	// Actions runs: every trigger and branch, newest first.
	var wr struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Event      string `json:"event"`
			Branch     string `json:"head_branch"`
			SHA        string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Actor      ghUser `json:"triggering_actor"`
		} `json:"workflow_runs"`
	}
	for p := 1; p <= 2; p++ {
		wr.Runs = nil
		if err := c.get(ctx, r+"/actions/runs", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(p)}}, &wr); err != nil {
			return snap, err
		}
		for _, x := range wr.Runs {
			snap.Runs = append(snap.Runs, WorkflowRun{ID: x.ID, Name: x.Name, Event: x.Event, Branch: x.Branch, SHA: x.SHA,
				Actor: x.Actor.Login, Status: x.Status, Conclusion: x.Conclusion})
		}
		if len(wr.Runs) < 100 || since == "" {
			break
		}
	}

	// who did what on issues and PRs (repo-wide timeline events, newest first)
	var ev []struct {
		ID        int64                   `json:"id"`
		Event     string                  `json:"event"`
		Actor     ghUser                  `json:"actor"`
		Label     *struct{ Name string }  `json:"label"`
		Assignee  *ghUser                 `json:"assignee"`
		Milestone *struct{ Title string } `json:"milestone"`
		Rename    *struct{ To string }    `json:"rename"`
		Issue     struct{ Number int }    `json:"issue"`
	}
	if err := c.get(ctx, r+"/issues/events", url.Values{"per_page": {"100"}}, &ev); err != nil {
		return snap, err
	}
	for _, x := range ev {
		d := ""
		switch {
		case x.Label != nil:
			d = x.Label.Name
		case x.Assignee != nil:
			d = x.Assignee.Login
		case x.Milestone != nil:
			d = x.Milestone.Title
		case x.Rename != nil:
			d = x.Rename.To
		}
		snap.Actions = append(snap.Actions, RepoEvent{ID: x.ID, Number: x.Issue.Number, Event: x.Event, Actor: x.Actor.Login, Detail: d})
	}

	var rel []struct {
		ID         int64  `json:"id"`
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		Author     ghUser `json:"author"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := c.get(ctx, r+"/releases", url.Values{"per_page": {"30"}}, &rel); err != nil {
		return snap, err
	}
	for _, x := range rel {
		if !x.Draft {
			snap.Releases = append(snap.Releases, Release{ID: x.ID, Tag: x.Tag, Name: x.Name, Author: x.Author.Login, Prerelease: x.Prerelease})
		}
	}
	return snap, nil
}

func fetchPR(ctx context.Context, c *client, n int) (ObservedPR, error) {
	r := fmt.Sprintf("/repos/%s/pulls/%d", c.repo, n)
	var p ghPR
	if err := c.get(ctx, r, nil, &p); err != nil {
		return ObservedPR{}, err
	}
	o := ObservedPR{Number: p.Number, Title: p.Title, State: p.State, Draft: p.Draft, Merged: p.Merged || p.MergedAt != nil,
		HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Mergeable: p.MergeableState, Author: p.User.Login}
	for _, l := range p.Labels {
		o.Labels = append(o.Labels, l.Name)
	}
	rv, err := pages[ghReview](ctx, c, r+"/reviews", nil, 3)
	if err != nil {
		return o, err
	}
	for _, x := range rv {
		if x.State == "PENDING" {
			continue // a draft review is invisible to others
		}
		o.Reviews = append(o.Reviews, Review{ID: x.ID, User: x.User.Login, State: x.State, CommitID: x.CommitID})
	}
	if o.State != "open" || o.HeadSHA == "" {
		return o, nil // checks of a closed PR do not matter
	}
	var cr struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs", c.repo, o.HeadSHA), url.Values{"per_page": {"100"}}, &cr); err != nil {
		return o, err
	}
	for _, x := range cr.CheckRuns {
		o.Checks = append(o.Checks, Check{Name: x.Name, Status: x.Status, Conclusion: x.Conclusion})
	}
	var st struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/status", c.repo, o.HeadSHA), nil, &st); err != nil {
		return o, err
	}
	for _, x := range st.Statuses { // commit statuses (older integrations): pending | success | failure | error
		ck := Check{Name: x.Context, Status: "completed", Conclusion: x.State}
		if x.State == "pending" {
			ck = Check{Name: x.Context, Status: "in_progress"}
		}
		o.Checks = append(o.Checks, ck)
	}
	return o, nil
}

func lastNum(u string) int {
	n, _ := strconv.Atoi(u[strings.LastIndex(u, "/")+1:])
	return n
}

// botMarker starts every comment the bot writes (CLAUDE.md §3), also when it is posted from the owner account.
const botMarker = "🤖"

type ghIssueComment struct {
	User      ghUser `json:"user"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func toObservedIssue(it ghIssue) ObservedIssue {
	oi := ObservedIssue{Number: it.Number, Title: it.Title, State: it.State}
	for _, a := range it.Assignees {
		oi.Assignees = append(oi.Assignees, a.Login)
	}
	for _, l := range it.Labels {
		oi.Labels = append(oi.Labels, l.Name)
	}
	if it.Milestone != nil {
		oi.Milestone = it.Milestone.Title
	}
	return oi
}
