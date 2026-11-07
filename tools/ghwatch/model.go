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
const SchemaVersion = 1

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
	At       string          `json:"at"` // RFC 3339, the time the fetch started; becomes the next cursor
	PRs      []ObservedPR    `json:"prs"`
	Issues   []ObservedIssue `json:"issues"`
	Comments []Comment       `json:"comments"`
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
}

// State is everything ghwatch remembers between runs. Maps are keyed by the decimal number so the JSON is stable.
type State struct {
	Version    int                   `json:"version"`
	Repo       string                `json:"repo"`
	Owner      string                `json:"owner"`
	Cursor     string                `json:"cursor"`
	PRs        map[string]PRState    `json:"prs"`
	Issues     map[string]IssueState `json:"issues"`
	CommentIDs []int64               `json:"comment_ids"` // reported comment ids, sorted, capped (see maxCommentIDs)
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
