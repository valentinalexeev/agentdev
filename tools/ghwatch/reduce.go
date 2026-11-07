package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Event kinds. This list is the whole vocabulary; the wrapper and tests depend on it.
const (
	EvPRNew           = "pr_new"
	EvPRLifecycle     = "pr_lifecycle" // draft/open/merged/closed changes
	EvPRPushed        = "pr_pushed"    // head commit changed
	EvPRRetitled      = "pr_retitled"
	EvPRRebased       = "pr_base_changed"
	EvPRChecks        = "pr_checks"
	EvPRReviewState   = "pr_review_state" // the owner's review state on the head changed
	EvPRReview        = "pr_review"       // a review was submitted (anyone)
	EvPRMergeable     = "pr_mergeable"
	EvPRMergeReady    = "pr_merge_ready" // To = "true" | "false"
	EvLabelAdded      = "label_added"
	EvLabelRemoved    = "label_removed"
	EvIssueNew        = "issue_new"
	EvIssueLifecycle  = "issue_lifecycle"
	EvIssueRetitled   = "issue_retitled"
	EvIssueAssigned   = "issue_assigned"
	EvIssueUnassigned = "issue_unassigned"
	EvIssueMilestone  = "issue_milestone"
	EvComment         = "comment"
	EvRunNew          = "run_new"       // a workflow run appeared (any trigger)
	EvRunStatus       = "run_status"    // queued -> in_progress
	EvRunConcluded    = "run_concluded" // completed, To = conclusion
	EvAction          = "action"        // who did what on an issue/PR (closed, labeled, assigned, merged...)
	EvRelease         = "release"
	EvDecision        = "decision" // issue decision state: none | asked | answered
	EvAnomaly         = "anomaly"
)

// maxCommentIDs bounds the remembered comment ids. Ids grow monotonically, so the oldest are dropped, and any comment
// with an id below the lowest kept one is treated as already seen (the fetcher only asks for recent comments).
const maxCommentIDs = 5000

// edge is one allowed transition.
type edge struct{ from, to string }

// prLifecycleEdges: what a pull request may do between two observations. merged is terminal.
var prLifecycleEdges = map[edge]bool{
	{PRDraft, PROpen}: true, {PROpen, PRDraft}: true,
	{PRDraft, PRMerged}: true, {PROpen, PRMerged}: true,
	{PRDraft, PRClosed}: true, {PROpen, PRClosed}: true,
	{PRClosed, PROpen}: true, {PRClosed, PRDraft}: true, // reopened
}

var issueLifecycleEdges = map[edge]bool{
	{IssueOpen, IssueClosed}: true, {IssueClosed, IssueOpen}: true,
}

// Workflow run statuses after normalization.
const (
	RunQueued     = "queued"
	RunInProgress = "in_progress"
	RunCompleted  = "completed"
)

// runEdges: a run only moves forward. completed is terminal except for a re-run, which GitHub reports as a new
// attempt on the same id (queued or in_progress again): allowed and reported, not an anomaly.
var runEdges = map[edge]bool{
	{RunQueued, RunInProgress}: true, {RunQueued, RunCompleted}: true, {RunInProgress, RunCompleted}: true,
	{RunCompleted, RunQueued}: true, {RunCompleted, RunInProgress}: true,
}

// maxRuns bounds the tracked runs; completed runs are pruned oldest id first.
const maxRuns = 200

// actionEvents: the timeline events worth reporting. Others (subscribed, mentioned, referenced...) are noise.
var actionEvents = map[string]bool{
	"closed": true, "reopened": true, "merged": true, "labeled": true, "unlabeled": true,
	"assigned": true, "unassigned": true, "milestoned": true, "demilestoned": true, "renamed": true,
	"review_requested": true, "review_request_removed": true, "ready_for_review": true, "convert_to_draft": true,
	"locked": true, "unlocked": true, "pinned": true, "transferred": true, "head_ref_force_pushed": true,
	"head_ref_deleted": true, "base_ref_changed": true, "auto_merge_enabled": true, "auto_merge_disabled": true,
}

// checkEdges on the SAME head commit. A new head resets the checks and is not checked against this table.
// Going back to "none" on the same commit means check results disappeared: an anomaly.
var checkEdges = map[edge]bool{
	{ChecksNone, ChecksPending}: true, {ChecksNone, ChecksSuccess}: true, {ChecksNone, ChecksFailure}: true,
	{ChecksPending, ChecksSuccess}: true, {ChecksPending, ChecksFailure}: true,
	{ChecksSuccess, ChecksPending}: true, {ChecksFailure, ChecksPending}: true, // re-run, or a late check started
	{ChecksSuccess, ChecksFailure}: true, {ChecksFailure, ChecksSuccess}: true, // a re-run finished between polls
}

// reviewEdges on the SAME head commit. On a new head an approval becomes stale; that is derived, not a free move.
var reviewEdges = map[edge]bool{
	{ReviewNone, ReviewApproved}: true, {ReviewNone, ReviewChangesRequested}: true, {ReviewNone, ReviewCommented}: true,
	{ReviewCommented, ReviewApproved}: true, {ReviewCommented, ReviewChangesRequested}: true,
	{ReviewChangesRequested, ReviewApproved}: true, {ReviewApproved, ReviewChangesRequested}: true,
	{ReviewApprovedStale, ReviewApproved}: true, {ReviewApprovedStale, ReviewChangesRequested}: true,
	{ReviewApproved, ReviewNone}: true, {ReviewChangesRequested, ReviewNone}: true, // dismissed
	{ReviewApprovedStale, ReviewNone}: true,
}

// NewState is an empty state; the first Reduce on it is a baseline.
func NewState(repo, owner string) State {
	return State{Version: SchemaVersion, Repo: repo, Owner: owner, PRs: map[string]PRState{}, Issues: map[string]IssueState{}, Runs: map[string]RunState{}}
}

// Reduce applies one snapshot. It is pure: it does not read the clock, the network or the disk, and its output
// depends only on its input (maps are always walked in sorted key order). On a fresh state (empty Cursor) it records
// everything and returns no events.
func Reduce(prev State, snap Snapshot) (State, []Event) {
	next := cloneState(prev)
	baseline := prev.Cursor == ""
	var ev []Event
	emit := func(e Event) {
		if !baseline {
			ev = append(ev, e)
		}
	}

	prs := dedupePRs(snap.PRs, emit)
	for _, o := range prs {
		key := strconv.Itoa(o.Number)
		obs := derivePR(o, prev.Owner)
		old, known := prev.PRs[key]
		if !known {
			emit(Event{Kind: EvPRNew, Number: o.Number, To: obs.Lifecycle, Actor: o.Author, Owner: o.Author == prev.Owner,
				Text: fmt.Sprintf("PR #%d new (%s) by %s: %s", o.Number, obs.Lifecycle, o.Author, o.Title)})
			for _, r := range sortedReviews(o.Reviews) {
				emit(reviewEvent(o.Number, r, prev.Owner))
			}
			next.PRs[key] = obs
			continue
		}
		next.PRs[key] = transitionPR(o.Number, old, obs, o.Reviews, prev.Owner, emit)
	}

	issues := dedupeIssues(snap.Issues, emit)
	lastHuman := map[int]LastHuman{}
	for _, h := range snap.LastHuman {
		lastHuman[h.Number] = h
	}
	for _, o := range issues {
		key := strconv.Itoa(o.Number)
		obs := IssueState{Title: o.Title, Lifecycle: o.State, Assignees: sortedSet(o.Assignees), Labels: sortedSet(o.Labels), Milestone: o.Milestone}
		obs.Decision = decisionOf(o.Number, obs, lastHuman, prev.Owner)
		if obs.Lifecycle != IssueOpen && obs.Lifecycle != IssueClosed {
			emit(Event{Kind: EvAnomaly, Number: o.Number, To: o.State, Text: fmt.Sprintf("issue #%d has unknown state %q", o.Number, o.State)})
		}
		old, known := prev.Issues[key]
		if !known {
			emit(Event{Kind: EvIssueNew, Number: o.Number, To: obs.Lifecycle,
				Text: fmt.Sprintf("issue #%d new (%s), assigned to [%s]: %s", o.Number, obs.Lifecycle, strings.Join(obs.Assignees, ","), o.Title)})
			if obs.Decision == DecisionAnswered {
				emit(decisionEvent(o.Number, DecisionNone, obs))
			}
			next.Issues[key] = obs
			continue
		}
		if old.Decision == "" {
			old.Decision = DecisionNone // state from before schema 3
		}
		transitionIssue(o.Number, old, obs, prev.Owner, emit)
		next.Issues[key] = obs
	}

	next.CommentIDs = reduceComments(prev.CommentIDs, snap.Comments, prev.Owner, emit)
	next.Runs = reduceRuns(prev.Runs, snap.Runs, prev.Owner, emit)
	next.ActionIDs = reduceActions(prev.ActionIDs, snap.Actions, prev.Owner, emit)
	next.ReleaseIDs = reduceReleases(prev.ReleaseIDs, snap.Releases, prev.Owner, emit)
	if snap.At != "" {
		next.Cursor = snap.At
	} else if next.Cursor == "" {
		next.Cursor = "baseline" // a snapshot without a time still ends the baseline
	}
	return next, ev
}

func transitionPR(n int, old, obs PRState, reviews []Review, owner string, emit func(Event)) PRState {
	// #242: the fetcher does not read checks of a merged or closed PR; keep the last known value instead of reading
	// the empty list as "checks disappeared".
	if (obs.Lifecycle == PRMerged || obs.Lifecycle == PRClosed) && obs.Checks == ChecksNone && obs.HeadSHA == old.HeadSHA {
		obs.Checks, obs.FailedNames = old.Checks, old.FailedNames
	}
	if old.Lifecycle != obs.Lifecycle {
		e := Event{Kind: EvPRLifecycle, Number: n, From: old.Lifecycle, To: obs.Lifecycle,
			Text: fmt.Sprintf("PR #%d %s -> %s: %s", n, old.Lifecycle, obs.Lifecycle, obs.Title)}
		if !prLifecycleEdges[edge{old.Lifecycle, obs.Lifecycle}] {
			e.Kind, e.Text = EvAnomaly, fmt.Sprintf("PR #%d illegal transition %s -> %s", n, old.Lifecycle, obs.Lifecycle)
		}
		emit(e)
	}
	if old.Title != obs.Title {
		emit(Event{Kind: EvPRRetitled, Number: n, From: old.Title, To: obs.Title, Text: fmt.Sprintf("PR #%d retitled: %s", n, obs.Title)})
	}
	if old.BaseRef != obs.BaseRef && old.BaseRef != "" {
		emit(Event{Kind: EvPRRebased, Number: n, From: old.BaseRef, To: obs.BaseRef, Text: fmt.Sprintf("PR #%d base %s -> %s", n, old.BaseRef, obs.BaseRef)})
	}
	pushed := old.HeadSHA != obs.HeadSHA
	if pushed {
		emit(Event{Kind: EvPRPushed, Number: n, From: short(old.HeadSHA), To: short(obs.HeadSHA),
			Text: fmt.Sprintf("PR #%d pushed %s -> %s", n, short(old.HeadSHA), short(obs.HeadSHA))})
	}
	if old.Checks != obs.Checks || (pushed && obs.Checks != ChecksNone) {
		e := Event{Kind: EvPRChecks, Number: n, From: old.Checks, To: obs.Checks, Text: checksText(n, obs)}
		if pushed {
			e.From = ChecksNone // a new head starts from no checks
		} else if !checkEdges[edge{old.Checks, obs.Checks}] {
			e.Kind, e.Text = EvAnomaly, fmt.Sprintf("PR #%d checks %s -> %s on the same commit %s", n, old.Checks, obs.Checks, short(obs.HeadSHA))
		}
		if e.From != e.To || e.Kind == EvAnomaly {
			emit(e)
		}
	} else if obs.Checks == ChecksFailure && !equalStrings(old.FailedNames, obs.FailedNames) {
		emit(Event{Kind: EvPRChecks, Number: n, From: old.Checks, To: obs.Checks, Text: checksText(n, obs)})
	}
	seen := map[int64]bool{}
	for _, id := range old.ReviewIDs {
		seen[id] = true
	}
	for _, r := range sortedReviews(reviews) {
		if !seen[r.ID] {
			emit(reviewEvent(n, r, owner))
		}
	}
	if old.Review != obs.Review {
		e := Event{Kind: EvPRReviewState, Number: n, From: old.Review, To: obs.Review, Owner: true,
			Text: fmt.Sprintf("PR #%d owner review %s -> %s (head %s)", n, old.Review, obs.Review, short(obs.HeadSHA))}
		staleByPush := pushed && old.Review == ReviewApproved && obs.Review == ReviewApprovedStale
		if !staleByPush && !pushed && !reviewEdges[edge{old.Review, obs.Review}] {
			e.Kind, e.Text = EvAnomaly, fmt.Sprintf("PR #%d owner review %s -> %s without a push", n, old.Review, obs.Review)
		}
		emit(e)
	}
	if old.Mergeable != obs.Mergeable && obs.Mergeable != MergeUnknown {
		emit(Event{Kind: EvPRMergeable, Number: n, From: old.Mergeable, To: obs.Mergeable, Text: fmt.Sprintf("PR #%d mergeable: %s", n, obs.Mergeable)})
	}
	if obs.Mergeable == MergeUnknown && old.Mergeable != MergeUnknown && !pushed {
		obs.Mergeable = old.Mergeable // GitHub is recomputing; keep the last known value instead of flapping
		obs.MergeReady = mergeReady(obs)
	}
	labelEvents(n, "PR", old.Labels, obs.Labels, emit)
	if old.MergeReady != obs.MergeReady {
		emit(Event{Kind: EvPRMergeReady, Number: n, From: strconv.FormatBool(old.MergeReady), To: strconv.FormatBool(obs.MergeReady), Owner: obs.MergeReady,
			Text: fmt.Sprintf("PR #%d merge ready: %t", n, obs.MergeReady)})
	}
	return obs
}

func transitionIssue(n int, old, obs IssueState, owner string, emit func(Event)) {
	if old.Lifecycle != obs.Lifecycle {
		e := Event{Kind: EvIssueLifecycle, Number: n, From: old.Lifecycle, To: obs.Lifecycle, Text: fmt.Sprintf("issue #%d %s -> %s: %s", n, old.Lifecycle, obs.Lifecycle, obs.Title)}
		if !issueLifecycleEdges[edge{old.Lifecycle, obs.Lifecycle}] {
			e.Kind, e.Text = EvAnomaly, fmt.Sprintf("issue #%d illegal transition %s -> %s", n, old.Lifecycle, obs.Lifecycle)
		}
		emit(e)
	}
	if old.Title != obs.Title {
		emit(Event{Kind: EvIssueRetitled, Number: n, From: old.Title, To: obs.Title, Text: fmt.Sprintf("issue #%d retitled: %s", n, obs.Title)})
	}
	add, del := diffSets(old.Assignees, obs.Assignees)
	for _, a := range add {
		emit(Event{Kind: EvIssueAssigned, Number: n, To: a, Text: fmt.Sprintf("issue #%d assigned to %s: %s", n, a, obs.Title)})
	}
	for _, a := range del {
		emit(Event{Kind: EvIssueUnassigned, Number: n, From: a, Text: fmt.Sprintf("issue #%d unassigned from %s", n, a)})
	}
	if old.Milestone != obs.Milestone {
		emit(Event{Kind: EvIssueMilestone, Number: n, From: old.Milestone, To: obs.Milestone, Text: fmt.Sprintf("issue #%d milestone %q -> %q", n, old.Milestone, obs.Milestone)})
	}
	labelEvents(n, "issue", old.Labels, obs.Labels, emit)
	if old.Decision != obs.Decision {
		e := decisionEvent(n, old.Decision, obs)
		if !decisionEdges[edge{old.Decision, obs.Decision}] {
			e.Kind, e.Owner = EvAnomaly, false
			e.Text = fmt.Sprintf("issue #%d decision %s -> %s", n, old.Decision, obs.Decision)
		}
		emit(e)
	}
}

// decisionEdges: every move is legal; the table documents them and keeps the reducer strict for future states.
// none -> answered happens when the label is set and the owner already commented (rare, still reported).
var decisionEdges = map[edge]bool{
	{DecisionNone, DecisionAsked}: true, {DecisionNone, DecisionAnswered}: true,
	{DecisionAsked, DecisionAnswered}: true, {DecisionAsked, DecisionNone}: true,
	{DecisionAnswered, DecisionAsked}: true, {DecisionAnswered, DecisionNone}: true,
}

// decisionOf: none without the needs-decision label or on a closed issue; answered when the latest unmarked
// comment is the owner's and newer than the latest bot-marked one; asked otherwise. Without LastHuman data for an
// open labelled issue it stays asked (the fetcher always sends it; a missing entry is not an answer).
func decisionOf(n int, s IssueState, lh map[int]LastHuman, owner string) string {
	labelled := false
	for _, l := range s.Labels {
		if l == "needs-decision" {
			labelled = true
		}
	}
	if !labelled || s.Lifecycle != IssueOpen {
		return DecisionNone
	}
	if h, ok := lh[n]; ok && h.User == owner && h.HumanAt > h.BotAt {
		return DecisionAnswered
	}
	return DecisionAsked
}

func decisionEvent(n int, from string, s IssueState) Event {
	t := fmt.Sprintf("issue #%d decision %s -> %s: %s", n, from, s.Decision, s.Title)
	if s.Decision == DecisionAnswered {
		t = fmt.Sprintf("issue #%d: the owner answered and needs-decision is still set: %s", n, s.Title)
	}
	return Event{Kind: EvDecision, Number: n, From: from, To: s.Decision, Owner: s.Decision == DecisionAnswered, Text: t}
}

func normRunStatus(s string) string {
	switch s {
	case RunInProgress:
		return RunInProgress
	case RunCompleted:
		return RunCompleted
	}
	return RunQueued // queued, waiting, requested, pending
}

func reduceRuns(prev map[string]RunState, runs []WorkflowRun, owner string, emit func(Event)) map[string]RunState {
	next := make(map[string]RunState, len(prev))
	for k, v := range prev {
		next[k] = v
	}
	sorted := append([]WorkflowRun(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for i, r := range sorted {
		if i > 0 && sorted[i-1].ID == r.ID {
			continue
		}
		key := strconv.FormatInt(r.ID, 10)
		obs := RunState{Name: r.Name, Branch: r.Branch, Status: normRunStatus(r.Status), Conclusion: r.Conclusion}
		if obs.Status != RunCompleted {
			obs.Conclusion = ""
		}
		old, known := prev[key]
		label := fmt.Sprintf("run %q on %s (%s)", r.Name, r.Branch, r.Event)
		if !known {
			k, to := EvRunNew, obs.Status
			if obs.Status == RunCompleted {
				k, to = EvRunConcluded, obs.Conclusion
			}
			emit(Event{Kind: k, To: to, Actor: r.Actor, Owner: r.Actor == owner,
				Text: fmt.Sprintf("%s %s by %s on %s", label, to, r.Actor, short(r.SHA))})
			next[key] = obs
			continue
		}
		switch {
		case old.Status != obs.Status && !runEdges[edge{old.Status, obs.Status}]:
			emit(Event{Kind: EvAnomaly, From: old.Status, To: obs.Status, Text: fmt.Sprintf("%s went %s -> %s", label, old.Status, obs.Status)})
		case old.Status != obs.Status && obs.Status == RunCompleted:
			emit(Event{Kind: EvRunConcluded, From: old.Status, To: obs.Conclusion, Actor: r.Actor, Owner: r.Actor == owner,
				Text: fmt.Sprintf("%s %s on %s", label, obs.Conclusion, short(r.SHA))})
		case old.Status != obs.Status:
			emit(Event{Kind: EvRunStatus, From: old.Status, To: obs.Status, Text: fmt.Sprintf("%s %s -> %s", label, old.Status, obs.Status)})
		case obs.Status == RunCompleted && old.Conclusion != obs.Conclusion:
			emit(Event{Kind: EvAnomaly, From: old.Conclusion, To: obs.Conclusion,
				Text: fmt.Sprintf("%s conclusion changed %s -> %s without a re-run", label, old.Conclusion, obs.Conclusion)})
		}
		next[key] = obs
	}
	if len(next) > maxRuns { // keep every unfinished run, and the newest completed ones
		var done []int64
		for k, v := range next {
			if v.Status == RunCompleted {
				id, _ := strconv.ParseInt(k, 10, 64)
				done = append(done, id)
			}
		}
		sort.Slice(done, func(i, j int) bool { return done[i] < done[j] })
		for _, id := range done {
			if len(next) <= maxRuns {
				break
			}
			delete(next, strconv.FormatInt(id, 10))
		}
	}
	return next
}

// seenIDs: items are reported once by id. With a full list, ids at or below the oldest kept one count as seen.
func seenIDs(prev []int64, max int) (map[int64]bool, int64) {
	seen := make(map[int64]bool, len(prev))
	for _, id := range prev {
		seen[id] = true
	}
	var floor int64 = -1
	if len(prev) >= max && len(prev) > 0 {
		floor = prev[0]
	}
	return seen, floor
}

func capIDs(ids []int64, max int) []int64 {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > max {
		ids = ids[len(ids)-max:]
	}
	return ids
}

func reduceActions(prev []int64, evs []RepoEvent, owner string, emit func(Event)) []int64 {
	seen, floor := seenIDs(prev, maxCommentIDs)
	sorted := append([]RepoEvent(nil), evs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	ids := append([]int64(nil), prev...)
	for _, e := range sorted {
		if seen[e.ID] || e.ID <= floor {
			continue
		}
		seen[e.ID] = true
		ids = append(ids, e.ID)
		if !actionEvents[e.Event] {
			continue // remembered, not reported
		}
		t := fmt.Sprintf("#%d %s by %s", e.Number, e.Event, e.Actor)
		if e.Detail != "" {
			t += ": " + e.Detail
		}
		emit(Event{Kind: EvAction, Number: e.Number, To: e.Event, Actor: e.Actor, Owner: e.Actor == owner, Text: t})
	}
	return capIDs(ids, maxCommentIDs)
}

const maxReleaseIDs = 1000

func reduceReleases(prev []int64, rs []Release, owner string, emit func(Event)) []int64 {
	seen, floor := seenIDs(prev, maxReleaseIDs)
	sorted := append([]Release(nil), rs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	ids := append([]int64(nil), prev...)
	for _, r := range sorted {
		if seen[r.ID] || r.ID <= floor {
			continue
		}
		seen[r.ID] = true
		ids = append(ids, r.ID)
		kind := "release"
		if r.Prerelease {
			kind = "pre-release"
		}
		emit(Event{Kind: EvRelease, To: r.Tag, Actor: r.Author, Owner: r.Author == owner,
			Text: fmt.Sprintf("%s %s published by %s: %s", kind, r.Tag, r.Author, r.Name)})
	}
	return capIDs(ids, maxReleaseIDs)
}

func reduceComments(prev []int64, cs []Comment, owner string, emit func(Event)) []int64 {
	seen := make(map[int64]bool, len(prev))
	var floor int64 = -1
	if len(prev) >= maxCommentIDs {
		floor = prev[0]
	}
	for _, id := range prev {
		seen[id] = true
	}
	sorted := append([]Comment(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	ids := append([]int64(nil), prev...)
	for _, c := range sorted {
		if seen[c.ID] || c.ID <= floor {
			continue
		}
		seen[c.ID] = true
		ids = append(ids, c.ID)
		where := fmt.Sprintf("#%d", c.Number)
		if c.Inline {
			where = fmt.Sprintf("PR #%d %s", c.Number, c.Path)
		}
		emit(Event{Kind: EvComment, Number: c.Number, Actor: c.User, Owner: c.User == owner, To: strconv.FormatInt(c.ID, 10),
			Text: fmt.Sprintf("%s %s: %s", where, c.User, oneLine(c.Body, 160))})
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > maxCommentIDs {
		ids = ids[len(ids)-maxCommentIDs:]
	}
	return ids
}

// derivePR turns an observation into the tracked state: lifecycle, aggregated checks, the owner's review on the
// head, and merge readiness.
func derivePR(o ObservedPR, owner string) PRState {
	life := PROpen
	switch {
	case o.Merged:
		life = PRMerged
	case o.State == "closed":
		life = PRClosed
	case o.Draft:
		life = PRDraft
	}
	checks, failed := aggregateChecks(o.Checks)
	s := PRState{Title: o.Title, Lifecycle: life, HeadSHA: o.HeadSHA, BaseRef: o.BaseRef, Checks: checks, FailedNames: failed,
		Review: ownerReview(o.Reviews, owner, o.HeadSHA), Mergeable: normMergeable(o.Mergeable), Labels: sortedSet(o.Labels)}
	for _, r := range o.Reviews {
		s.ReviewIDs = append(s.ReviewIDs, r.ID)
	}
	sort.Slice(s.ReviewIDs, func(i, j int) bool { return s.ReviewIDs[i] < s.ReviewIDs[j] })
	s.MergeReady = mergeReady(s)
	return s
}

// mergeReady is CLAUDE.md §3: open, the owner's Approve on the current head, green checks, no conflict.
func mergeReady(s PRState) bool {
	return s.Lifecycle == PROpen && s.Review == ReviewApproved && s.Checks == ChecksSuccess && s.Mergeable == MergeClean
}

// aggregateChecks: any failure wins, then anything unfinished, then success. neutral and skipped count as success.
func aggregateChecks(cs []Check) (string, []string) {
	if len(cs) == 0 {
		return ChecksNone, nil
	}
	var failed []string
	pending := false
	for _, c := range cs {
		if c.Status != "completed" {
			pending = true
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale", "error":
			failed = append(failed, c.Name)
		default:
			pending = true
		}
	}
	if len(failed) > 0 {
		return ChecksFailure, sortedSet(failed)
	}
	if pending {
		return ChecksPending, nil
	}
	return ChecksSuccess, nil
}

// ownerReview: the owner's latest deciding review (APPROVED, CHANGES_REQUESTED, DISMISSED); COMMENTED only counts
// when there is nothing else. Reviews are ordered by id, which GitHub assigns in submission order.
func ownerReview(rs []Review, owner, head string) string {
	state := ReviewNone
	for _, r := range sortedReviews(rs) {
		if r.User != owner {
			continue
		}
		switch r.State {
		case "APPROVED":
			if r.CommitID == head {
				state = ReviewApproved
			} else {
				state = ReviewApprovedStale
			}
		case "CHANGES_REQUESTED":
			state = ReviewChangesRequested
		case "DISMISSED":
			state = ReviewNone
		case "COMMENTED":
			if state == ReviewNone {
				state = ReviewCommented
			}
		}
	}
	return state
}

func normMergeable(m string) string {
	switch m {
	case MergeClean, "unstable", "has_hooks":
		return MergeClean
	case MergeConflict, "dirty":
		return MergeConflict
	case MergeBlocked, "behind", "draft":
		return MergeBlocked
	}
	return MergeUnknown
}

func reviewEvent(n int, r Review, owner string) Event {
	return Event{Kind: EvPRReview, Number: n, Actor: r.User, Owner: r.User == owner, To: r.State,
		Text: fmt.Sprintf("PR #%d %s review by %s on %s", n, r.State, r.User, short(r.CommitID))}
}

func checksText(n int, s PRState) string {
	t := fmt.Sprintf("PR #%d checks %s on %s", n, s.Checks, short(s.HeadSHA))
	if len(s.FailedNames) > 0 {
		t += ": " + strings.Join(s.FailedNames, ", ")
	}
	return t
}

func labelEvents(n int, what string, old, cur []string, emit func(Event)) {
	add, del := diffSets(old, cur)
	for _, l := range add {
		emit(Event{Kind: EvLabelAdded, Number: n, To: l, Text: fmt.Sprintf("%s #%d label +%s", what, n, l)})
	}
	for _, l := range del {
		emit(Event{Kind: EvLabelRemoved, Number: n, From: l, Text: fmt.Sprintf("%s #%d label -%s", what, n, l)})
	}
}

// dedupePRs sorts by number and keeps the first of duplicates (stable), reporting the duplicate as an anomaly.
func dedupePRs(in []ObservedPR, emit func(Event)) []ObservedPR {
	s := append([]ObservedPR(nil), in...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].Number < s[j].Number })
	out := s[:0:0]
	for i, p := range s {
		if i > 0 && s[i-1].Number == p.Number {
			emit(Event{Kind: EvAnomaly, Number: p.Number, Text: fmt.Sprintf("PR #%d appears twice in one snapshot", p.Number)})
			continue
		}
		out = append(out, p)
	}
	return out
}

func dedupeIssues(in []ObservedIssue, emit func(Event)) []ObservedIssue {
	s := append([]ObservedIssue(nil), in...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].Number < s[j].Number })
	out := s[:0:0]
	for i, p := range s {
		if i > 0 && s[i-1].Number == p.Number {
			emit(Event{Kind: EvAnomaly, Number: p.Number, Text: fmt.Sprintf("issue #%d appears twice in one snapshot", p.Number)})
			continue
		}
		out = append(out, p)
	}
	return out
}

func sortedReviews(rs []Review) []Review {
	s := append([]Review(nil), rs...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].ID < s[j].ID })
	return s
}

func sortedSet(in []string) []string {
	m := map[string]bool{}
	for _, v := range in {
		if v != "" {
			m[v] = true
		}
	}
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func diffSets(old, cur []string) (add, del []string) {
	o, c := map[string]bool{}, map[string]bool{}
	for _, v := range old {
		o[v] = true
	}
	for _, v := range cur {
		c[v] = true
	}
	for _, v := range sortedSet(cur) {
		if !o[v] {
			add = append(add, v)
		}
	}
	for _, v := range sortedSet(old) {
		if !c[v] {
			del = append(del, v)
		}
	}
	return
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneState(s State) State {
	c := s
	c.PRs = make(map[string]PRState, len(s.PRs))
	for k, v := range s.PRs {
		c.PRs[k] = v
	}
	c.Issues = make(map[string]IssueState, len(s.Issues))
	for k, v := range s.Issues {
		c.Issues[k] = v
	}
	c.CommentIDs = append([]int64(nil), s.CommentIDs...)
	c.ActionIDs = append([]int64(nil), s.ActionIDs...)
	c.ReleaseIDs = append([]int64(nil), s.ReleaseIDs...)
	c.Runs = make(map[string]RunState, len(s.Runs))
	for k, v := range s.Runs {
		c.Runs[k] = v
	}
	return c
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}
