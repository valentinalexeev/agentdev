package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Gitea fetcher (#241 P8): the same Snapshot as fetch (github.go), so Reduce stays forge-neutral. Values are mapped
// to the GitHub words the reducer knows (review states, mergeable, check conclusions).
//
// Differences, all documented in README.md:
//   - Gitea has no repo-wide list of inline review comments: they are read per review of each fetched PR.
//   - Gitea has no repo-wide issue events (who closed, labeled, ...): Actions stays empty.
//   - Gitea has no "blocked" mergeable state: mergeable true is clean, false is conflict.
//   - Gitea Actions runs (/actions/runs, Gitea 1.24 and later) are read when the endpoint exists.

type gtUser struct {
	Login string `json:"login"`
}
type gtIssue struct {
	Number      int                     `json:"number"`
	Title       string                  `json:"title"`
	State       string                  `json:"state"`
	Assignees   []gtUser                `json:"assignees"`
	Labels      []ghLabel               `json:"labels"`
	Milestone   *struct{ Title string } `json:"milestone"`
	PullRequest *struct{}               `json:"pull_request"`
}
type gtPR struct {
	Number    int                       `json:"number"`
	Title     string                    `json:"title"`
	State     string                    `json:"state"`
	Draft     bool                      `json:"draft"`
	Merged    bool                      `json:"merged"`
	Mergeable bool                      `json:"mergeable"`
	User      gtUser                    `json:"user"`
	Labels    []ghLabel                 `json:"labels"`
	Head      struct{ SHA, Ref string } `json:"head"`
	Base      struct{ Ref string }      `json:"base"`
}
type gtReview struct {
	ID        int64  `json:"id"`
	User      gtUser `json:"user"`
	State     string `json:"state"` // APPROVED | REQUEST_CHANGES | COMMENT | PENDING | REQUEST_REVIEW
	CommitID  string `json:"commit_id"`
	Dismissed bool   `json:"dismissed"`
	Comments  int    `json:"comments_count"`
}
type gtComment struct {
	ID        int64  `json:"id"`
	User      gtUser `json:"user"`
	Body      string `json:"body"`
	IssueURL  string `json:"issue_url"`
	PRURL     string `json:"pull_request_url"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

// gtReviewState maps a Gitea review state to the GitHub word; "" = not a submitted review (skip it).
func gtReviewState(r gtReview) string {
	if r.Dismissed {
		return "DISMISSED"
	}
	switch r.State {
	case "APPROVED":
		return "APPROVED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	case "COMMENT":
		return "COMMENTED"
	}
	return ""
}

// gtTime: Gitea answers with a local offset (+01:00); the reducer compares RFC 3339 UTC strings.
func gtTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

func fetchGitea(ctx context.Context, c *client, cursor string, now time.Time) (Snapshot, error) {
	snap := Snapshot{At: now.UTC().Format(time.RFC3339)}
	r := "/repos/" + c.repo
	since := ""
	if t, err := time.Parse(time.RFC3339, cursor); err == nil {
		since = t.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	}

	// issues (type=issues) and PRs: open ones on a baseline, everything touched since the cursor otherwise
	q := url.Values{"state": {"open"}, "type": {"issues"}}
	if since != "" {
		q = url.Values{"state": {"all"}, "type": {"issues"}, "since": {since}}
	}
	items, err := pages[gtIssue](ctx, c, r+"/issues", q, 5)
	if err != nil {
		return snap, err
	}
	for _, it := range items {
		snap.Issues = append(snap.Issues, gtObservedIssue(it))
	}
	prNums := map[int]bool{}
	if since != "" {
		touched, err := pages[gtIssue](ctx, c, r+"/issues", url.Values{"state": {"all"}, "type": {"pulls"}, "since": {since}}, 5)
		if err != nil {
			return snap, err
		}
		for _, it := range touched {
			prNums[it.Number] = true
		}
	}
	open, err := pages[gtPR](ctx, c, r+"/pulls", url.Values{"state": {"open"}}, 3)
	if err != nil {
		return snap, err
	}
	for _, p := range open {
		prNums[p.Number] = true
	}
	for _, n := range sortedInts(prNums) {
		op, inline, err := fetchGiteaPR(ctx, c, n, since)
		if err != nil {
			return snap, err
		}
		snap.PRs = append(snap.PRs, op)
		snap.Comments = append(snap.Comments, inline...)
	}

	// conversation comments, repo-wide
	cq := url.Values{}
	maxPages := 1
	if since != "" {
		cq.Set("since", since)
		maxPages = 5
	}
	ic, err := pages[gtComment](ctx, c, r+"/issues/comments", cq, maxPages)
	if err != nil {
		return snap, err
	}
	for _, x := range ic {
		u := x.IssueURL
		if u == "" {
			u = x.PRURL
		}
		snap.Comments = append(snap.Comments, Comment{ID: x.ID, Number: lastNum(u), User: x.User.Login, Body: x.Body})
	}

	// decision state (#242), as on GitHub
	nd, err := pages[gtIssue](ctx, c, r+"/issues", url.Values{"state": {"open"}, "type": {"issues"}, "labels": {"needs-decision"}}, 2)
	if err != nil {
		return snap, err
	}
	have := map[int]bool{}
	for _, i := range snap.Issues {
		have[i.Number] = true
	}
	for _, it := range nd {
		if !have[it.Number] {
			snap.Issues = append(snap.Issues, gtObservedIssue(it))
		}
		cs, err := pages[gtComment](ctx, c, fmt.Sprintf("%s/issues/%d/comments", r, it.Number), nil, 5)
		if err != nil {
			return snap, err
		}
		h := LastHuman{Number: it.Number}
		for _, x := range cs { // oldest first
			if strings.HasPrefix(strings.TrimSpace(x.Body), botMarker) {
				h.BotAt = gtTime(x.CreatedAt)
			} else {
				h.User, h.HumanAt = x.User.Login, gtTime(x.CreatedAt)
			}
		}
		snap.LastHuman = append(snap.LastHuman, h)
	}

	// Gitea Actions runs (1.24+); an older Gitea answers 404: no runs, no error
	var wr struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Title      string `json:"display_title"`
			Path       string `json:"path"`
			Event      string `json:"event"`
			Branch     string `json:"head_branch"`
			SHA        string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Actor      gtUser `json:"trigger_actor"`
		} `json:"workflow_runs"`
	}
	if err := c.get(ctx, r+"/actions/runs", url.Values{"limit": {"50"}}, &wr); err != nil && !isNotFound(err) {
		return snap, err
	}
	for _, x := range wr.Runs {
		name := x.Path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			name = x.Title
		}
		snap.Runs = append(snap.Runs, WorkflowRun{ID: x.ID, Name: name, Event: x.Event, Branch: x.Branch, SHA: x.SHA,
			Actor: x.Actor.Login, Status: gtRunStatus(x.Status), Conclusion: x.Conclusion})
	}

	var rel []struct {
		ID         int64  `json:"id"`
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		Author     gtUser `json:"author"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := c.get(ctx, r+"/releases", url.Values{"limit": {"30"}}, &rel); err != nil {
		return snap, err
	}
	for _, x := range rel {
		if !x.Draft {
			snap.Releases = append(snap.Releases, Release{ID: x.ID, Tag: x.Tag, Name: x.Name, Author: x.Author.Login, Prerelease: x.Prerelease})
		}
	}
	return snap, nil
}

// fetchGiteaPR: one PR with its reviews and head statuses, plus the inline comments of reviews newer than since
// (all of them on a baseline).
func fetchGiteaPR(ctx context.Context, c *client, n int, since string) (ObservedPR, []Comment, error) {
	r := fmt.Sprintf("/repos/%s/pulls/%d", c.repo, n)
	var p gtPR
	if err := c.get(ctx, r, nil, &p); err != nil {
		return ObservedPR{}, nil, err
	}
	o := ObservedPR{Number: p.Number, Title: p.Title, State: p.State, Draft: p.Draft, Merged: p.Merged,
		HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Author: p.User.Login, Mergeable: MergeConflict}
	if p.Mergeable {
		o.Mergeable = MergeClean
	}
	for _, l := range p.Labels {
		o.Labels = append(o.Labels, l.Name)
	}
	rv, err := pages[gtReview](ctx, c, r+"/reviews", nil, 3)
	if err != nil {
		return o, nil, err
	}
	var inline []Comment
	for _, x := range rv {
		st := gtReviewState(x)
		if st == "" {
			continue // pending (draft) reviews and review requests are not reviews
		}
		o.Reviews = append(o.Reviews, Review{ID: x.ID, User: x.User.Login, State: st, CommitID: x.CommitID})
		if x.Comments == 0 {
			continue
		}
		var cs []gtComment
		if err := c.get(ctx, fmt.Sprintf("%s/reviews/%d/comments", r, x.ID), nil, &cs); err != nil {
			return o, nil, err
		}
		for _, k := range cs {
			if since != "" && gtTime(k.CreatedAt) < since {
				continue
			}
			inline = append(inline, Comment{ID: k.ID, Number: n, User: k.User.Login, Inline: true, Path: k.Path, Body: k.Body})
		}
	}
	if o.State != "open" || o.HeadSHA == "" {
		return o, inline, nil
	}
	var st struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
			Status  string `json:"status"` // Gitea before 1.x used "status"
		} `json:"statuses"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s/status", c.repo, o.HeadSHA), nil, &st); err != nil {
		return o, inline, err
	}
	for _, x := range st.Statuses { // pending | success | error | failure | warning
		s := x.State
		if s == "" {
			s = x.Status
		}
		switch s {
		case "pending":
			o.Checks = append(o.Checks, Check{Name: x.Context, Status: "in_progress"})
		case "warning":
			o.Checks = append(o.Checks, Check{Name: x.Context, Status: "completed", Conclusion: "neutral"})
		default:
			o.Checks = append(o.Checks, Check{Name: x.Context, Status: "completed", Conclusion: s})
		}
	}
	return o, inline, nil
}

// gtRunStatus maps Gitea run states to queued | in_progress | completed.
func gtRunStatus(s string) string {
	switch s {
	case "completed", "success", "failure", "cancelled", "skipped":
		return "completed"
	case "in_progress", "running":
		return "in_progress"
	}
	return "queued" // waiting, blocked, queued, unknown
}

func gtObservedIssue(it gtIssue) ObservedIssue {
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

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

var errNotFound = errors.New("not found")

func isNotFound(err error) bool { return errors.Is(err, errNotFound) }
