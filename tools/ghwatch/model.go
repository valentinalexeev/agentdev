// Package main is ghwatch: a deterministic state machine over GitHub activity for the session monitor (#232).
//
// The design splits I/O from logic:
//
//	fetch (github.go)  -> Snapshot   (I/O, the only non-deterministic part)
//	Reduce (reduce.go) -> State, []Event  (pure: same State and Snapshot give the same result, byte for byte)
//	store (store.go)   -> State on disk (atomic write)
//
// Every entity has an explicit transition table. A change the table does not allow is reported as an "anomaly"
// event and the observed value is still taken, so the monitor never gets stuck and never hides a surprise.
package main

// SchemaVersion of the persisted State. A State with another version is rejected, not migrated silently.
// 2: workflow runs, actor events and releases (#233 review).
// 3: issue decision state (#243 review).
const SchemaVersion = 3

// Lifecycle states of a pull request.
const (
	PRDraft  = "draft"
	PROpen   = "open"
	PRMerged = "merged"
	PRClosed = "closed"
)

// Lifecycle states of an issue.
const (
	IssueOpen   = "open"
	IssueClosed = "closed"
)

// Aggregated check state of one commit (check runs and commit statuses together).
const (
	ChecksNone    = "none"
	ChecksPending = "pending"
	ChecksSuccess = "success"
	ChecksFailure = "failure"
)

// The owner's review state on the PR's current head.
const (
	ReviewNone             = "none"
	ReviewApproved         = "approved"
	ReviewApprovedStale    = "approved_stale" // the owner approved an earlier commit; a push made it stale
	ReviewChangesRequested = "changes_requested"
	ReviewCommented        = "commented"
)

// Mergeability as GitHub reports it.
const (
	MergeClean    = "clean"
	MergeConflict = "conflict"
	MergeBlocked  = "blocked" // branch protection: approvals or required checks missing
	MergeUnknown  = "unknown" // GitHub is still computing it
)

// Check is one check run or one commit status on a commit.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | cancelled | skipped | timed_out | action_required | stale | ""
}

// Review is one submitted review.
type Review struct {
	ID       int64  `json:"id"`
	User     string `json:"user"`
	State    string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	CommitID string `json:"commit_id"`
}

// Comment is one issue or PR conversation comment, or one inline review comment.
type Comment struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"` // issue or PR number
	User   string `json:"user"`
	Inline bool   `json:"inline,omitempty"`
	Path   string `json:"path,omitempty"`
	Body   string `json:"body"`
}

// LastHuman is, per issue, the latest comment by a person and the latest bot-marked one (body starts with the bot
// marker, whoever posted it). The fetcher fills it for every open needs-decision issue on every poll, so the
// decision state never depends on having seen a comment event (#242).
type LastHuman struct {
	Number  int    `json:"number"`
	User    string `json:"user"`     // author of the latest unmarked comment
	HumanAt string `json:"human_at"` // its time (RFC 3339 UTC)
	BotAt   string `json:"bot_at"`   // time of the latest bot-marked comment, "" if none
}

// ObservedPR is what one fetch saw of a pull request.
type ObservedPR struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // open | closed (GitHub REST)
	Draft     bool     `json:"draft"`
	Merged    bool     `json:"merged"`
	HeadSHA   string   `json:"head_sha"`
	HeadRef   string   `json:"head_ref"`
	BaseRef   string   `json:"base_ref"`
	Mergeable string   `json:"mergeable"` // clean | conflict | blocked | unknown
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	Reviews   []Review `json:"reviews"`
	Checks    []Check  `json:"checks"` // on HeadSHA
}

// ObservedIssue is what one fetch saw of an issue (not a PR).
type ObservedIssue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // open | closed
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
	Milestone string   `json:"milestone"`
}

// Snapshot is one complete observation. Only the items in it are updated; items not in it keep their state
// (the fetcher asks for what changed since the cursor, plus every open PR).
type Snapshot struct {
	At        string          `json:"at"` // RFC 3339, the time the fetch started; becomes the next cursor
	PRs       []ObservedPR    `json:"prs"`
	Issues    []ObservedIssue `json:"issues"`
	Comments  []Comment       `json:"comments"`
	Runs      []WorkflowRun   `json:"runs"`       // GitHub Actions workflow runs (any branch, tags, schedules)
	LastHuman []LastHuman     `json:"last_human"` // per open needs-decision issue (decision state)
	Actions   []RepoEvent     `json:"actions"`    // issue/PR timeline events with the actor (who closed, labeled, assigned...)
	Releases  []Release       `json:"releases"`   // published releases and their tags
}

// WorkflowRun is one Actions run.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Event      string `json:"event"` // push | pull_request | schedule | workflow_dispatch | ...
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
	Actor      string `json:"actor"`
	Status     string `json:"status"`     // queued | in_progress | completed (waiting, requested, pending count as queued)
	Conclusion string `json:"conclusion"` // when completed
}

// RepoEvent is one issue or PR timeline event (repo-wide /issues/events): closed, reopened, labeled, assigned,
// milestoned, merged, review_requested, ... with the login of whoever did it.
type RepoEvent struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Event  string `json:"event"`
	Actor  string `json:"actor"`
	Detail string `json:"detail,omitempty"` // label name, assignee, milestone title
}

// Release is one published release (drafts are not visible to the bot).
type Release struct {
	ID         int64  `json:"id"`
	Tag        string `json:"tag"`
	Name       string `json:"name"`
	Author     string `json:"author"`
	Prerelease bool   `json:"prerelease"`
}

// RunState is the tracked state of a workflow run.
type RunState struct {
	Name       string `json:"name"`
	Branch     string `json:"branch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// PRState is the tracked state of a pull request.
type PRState struct {
	Title       string   `json:"title"`
	Lifecycle   string   `json:"lifecycle"`
	HeadSHA     string   `json:"head_sha"`
	BaseRef     string   `json:"base_ref"`
	Checks      string   `json:"checks"`
	FailedNames []string `json:"failed_checks,omitempty"`
	Review      string   `json:"review"`
	Mergeable   string   `json:"mergeable"`
	MergeReady  bool     `json:"merge_ready"`
	Labels      []string `json:"labels"`
	ReviewIDs   []int64  `json:"review_ids"` // every review already reported (sorted)
}

// IssueState is the tracked state of an issue.
type IssueState struct {
	Title     string   `json:"title"`
	Lifecycle string   `json:"lifecycle"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
	Milestone string   `json:"milestone"`
	Decision  string   `json:"decision"` // none | asked | answered (see decisionEdges)
}

// Decision states of an issue (#242). asked: needs-decision is set and the bot spoke last. answered: needs-decision
// is set and the owner spoke after the bot: an answer nobody recorded yet. none: no needs-decision label.
const (
	DecisionNone     = "none"
	DecisionAsked    = "asked"
	DecisionAnswered = "answered"
)

// State is everything ghwatch remembers between runs. Maps are keyed by the decimal number so the JSON is stable.
type State struct {
	Version    int                   `json:"version"`
	Repo       string                `json:"repo"`
	Owner      string                `json:"owner"`
	Cursor     string                `json:"cursor"`
	PRs        map[string]PRState    `json:"prs"`
	Issues     map[string]IssueState `json:"issues"`
	CommentIDs []int64               `json:"comment_ids"` // reported comment ids, sorted, capped (see maxCommentIDs)
	Runs       map[string]RunState   `json:"runs"`        // workflow runs not yet completed, plus completed ones until pruned
	ActionIDs  []int64               `json:"action_ids"`  // reported timeline event ids, sorted, capped
	ReleaseIDs []int64               `json:"release_ids"` // reported release ids, sorted
}

// Event is one reported change. Kind is a fixed vocabulary (see kinds in reduce.go); Text is for humans.
type Event struct {
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Owner  bool   `json:"owner,omitempty"` // the owner did it: needs the agent's attention
	Text   string `json:"text"`
}

func (s IssueState) answered() bool { return s.Decision == DecisionAnswered }
