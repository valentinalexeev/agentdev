package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const owner = "boss"

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
)

func pr(mod ...func(*ObservedPR)) ObservedPR {
	p := ObservedPR{Number: 7, Title: "feat: x", State: "open", HeadSHA: sha1, BaseRef: "main", Mergeable: "blocked", Author: "bot"}
	for _, m := range mod {
		m(&p)
	}
	return p
}

func green(p *ObservedPR) {
	p.Checks = []Check{{Name: "ci", Status: "completed", Conclusion: "success"}}
}
func red(p *ObservedPR) { p.Checks = []Check{{Name: "ci", Status: "completed", Conclusion: "failure"}} }
func running(p *ObservedPR) {
	p.Checks = []Check{{Name: "ci", Status: "in_progress"}}
}
func approvedOn(sha string, id int64) func(*ObservedPR) {
	return func(p *ObservedPR) {
		p.Reviews = append(p.Reviews, Review{ID: id, User: owner, State: "APPROVED", CommitID: sha})
	}
}

// base returns a state that already went through the baseline with the given snapshot.
func base(t *testing.T, s Snapshot) State {
	t.Helper()
	st, ev := Reduce(NewState("o/r", owner), s)
	if len(ev) != 0 {
		t.Fatalf("baseline emitted %d events: %+v", len(ev), ev)
	}
	return st
}

func kinds(ev []Event) []string {
	out := []string{}
	for _, e := range ev {
		out = append(out, e.Kind+":"+e.From+">"+e.To)
	}
	return out
}

func step(t *testing.T, st State, s Snapshot) (State, []string) {
	t.Helper()
	s.At = "2026-11-07T10:00:00Z"
	next, ev := Reduce(st, s)
	return next, kinds(ev)
}

func TestBaselineIsSilent(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(green)}, Issues: []ObservedIssue{{Number: 1, State: "open"}}, Comments: []Comment{{ID: 5, Number: 1, User: owner}}})
	if st.Cursor != "t0" || len(st.PRs) != 1 || len(st.Issues) != 1 || len(st.CommentIDs) != 1 {
		t.Fatalf("baseline state not recorded: %+v", st)
	}
}

func TestPRLifecycle(t *testing.T) {
	cases := []struct {
		name string
		from func(*ObservedPR)
		to   func(*ObservedPR)
		want []string
	}{
		{"open to draft", func(*ObservedPR) {}, func(p *ObservedPR) { p.Draft = true }, []string{"pr_lifecycle:open>draft"}},
		{"draft to open", func(p *ObservedPR) { p.Draft = true }, func(*ObservedPR) {}, []string{"pr_lifecycle:draft>open"}},
		{"merged", func(*ObservedPR) {}, func(p *ObservedPR) { p.State, p.Merged = "closed", true }, []string{"pr_lifecycle:open>merged"}},
		{"closed", func(*ObservedPR) {}, func(p *ObservedPR) { p.State = "closed" }, []string{"pr_lifecycle:open>closed"}},
		{"reopened", func(p *ObservedPR) { p.State = "closed" }, func(*ObservedPR) {}, []string{"pr_lifecycle:closed>open"}},
		{"merged is terminal", func(p *ObservedPR) { p.State, p.Merged = "closed", true }, func(*ObservedPR) {}, []string{"anomaly:merged>open"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(c.from)}})
			_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(c.to)}})
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestChecks(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr()}})
	st, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(running)}})
	want := []string{"pr_checks:none>pending"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	st, got = step(t, st, Snapshot{PRs: []ObservedPR{pr(red)}})
	if !reflect.DeepEqual(got, []string{"pr_checks:pending>failure"}) {
		t.Fatalf("got %v", got)
	}
	// same commit, results vanish: anomaly
	_, got = step(t, st, Snapshot{PRs: []ObservedPR{pr()}})
	if !reflect.DeepEqual(got, []string{"anomaly:failure>none"}) {
		t.Fatalf("got %v", got)
	}
}

func TestChecksFailedSetChange(t *testing.T) {
	two := func(p *ObservedPR) {
		p.Checks = []Check{{Name: "ci", Status: "completed", Conclusion: "failure"}, {Name: "codeql", Status: "completed", Conclusion: "failure"}}
	}
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(red)}})
	_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(two)}})
	if !reflect.DeepEqual(got, []string{"pr_checks:failure>failure"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPushMakesApprovalStaleAndResetsChecks(t *testing.T) {
	clean := func(p *ObservedPR) { p.Mergeable = "clean" }
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(green, clean, approvedOn(sha1, 1))}})
	if !st.PRs["7"].MergeReady {
		t.Fatalf("approved, green, clean PR should be merge ready: %+v", st.PRs["7"])
	}
	_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(clean, approvedOn(sha1, 1), func(p *ObservedPR) { p.HeadSHA = sha2 })}})
	want := []string{"pr_pushed:1111111>2222222", "pr_review_state:approved>approved_stale", "pr_merge_ready:true>false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeReady(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(green)}})
	st, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(green, approvedOn(sha1, 9))}})
	want := []string{"pr_review:>APPROVED", "pr_review_state:none>approved"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	_, got = step(t, st, Snapshot{PRs: []ObservedPR{pr(green, approvedOn(sha1, 9), func(p *ObservedPR) { p.Mergeable = "clean" })}})
	want = []string{"pr_mergeable:blocked>clean", "pr_merge_ready:false>true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeableUnknownDoesNotFlap(t *testing.T) {
	clean := func(p *ObservedPR) { p.Mergeable = "clean" }
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(green, clean, approvedOn(sha1, 1))}})
	st, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(green, approvedOn(sha1, 1), func(p *ObservedPR) { p.Mergeable = "" })}})
	if len(got) != 0 || !st.PRs["7"].MergeReady || st.PRs["7"].Mergeable != MergeClean {
		t.Fatalf("unknown mergeable should keep the last value: %v %+v", got, st.PRs["7"])
	}
}

func TestOwnerReviewOrdering(t *testing.T) {
	rs := []Review{
		{ID: 3, User: owner, State: "CHANGES_REQUESTED", CommitID: sha1},
		{ID: 1, User: owner, State: "APPROVED", CommitID: sha1},
		{ID: 4, User: owner, State: "COMMENTED", CommitID: sha1}, // a comment does not undo a decision
		{ID: 5, User: "someone", State: "APPROVED", CommitID: sha1},
	}
	if got := ownerReview(rs, owner, sha1); got != ReviewChangesRequested {
		t.Fatalf("got %s", got)
	}
	if got := ownerReview(rs[3:], owner, sha1); got != ReviewNone {
		t.Fatalf("non-owner approval counted: %s", got)
	}
	if got := ownerReview([]Review{{ID: 1, User: owner, State: "APPROVED", CommitID: sha1}, {ID: 2, User: owner, State: "DISMISSED"}}, owner, sha1); got != ReviewNone {
		t.Fatalf("dismissed approval counted: %s", got)
	}
}

func TestReviewWithoutPushAnomaly(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(approvedOn(sha2, 1))}}) // approved_stale
	st.PRs["7"] = func(p PRState) PRState { p.Review = ReviewCommented; return p }(st.PRs["7"])
	_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(approvedOn(sha2, 1))}})
	if !reflect.DeepEqual(got, []string{"anomaly:commented>approved_stale"}) {
		t.Fatalf("got %v", got)
	}
}

func TestIssues(t *testing.T) {
	i := ObservedIssue{Number: 3, Title: "t", State: "open", Assignees: []string{"bot"}, Labels: []string{"AI"}, Milestone: "1.3.0"}
	st := base(t, Snapshot{At: "t0", Issues: []ObservedIssue{i}})
	j := i
	j.Assignees, j.Labels, j.Milestone, j.State, j.Title = []string{owner}, []string{"AI", "needs-decision"}, "1.4.0", "closed", "t2"
	_, got := step(t, st, Snapshot{Issues: []ObservedIssue{j}})
	want := []string{"issue_lifecycle:open>closed", "issue_retitled:t>t2", "issue_assigned:>boss", "issue_unassigned:bot>",
		`issue_milestone:1.3.0>1.4.0`, "label_added:>needs-decision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestNewItems(t *testing.T) {
	st := base(t, Snapshot{At: "t0"})
	_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(approvedOn(sha1, 2))}, Issues: []ObservedIssue{{Number: 9, State: "open"}}})
	want := []string{"pr_new:>open", "pr_review:>APPROVED", "issue_new:>open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCommentsOnceAndOrdered(t *testing.T) {
	st := base(t, Snapshot{At: "t0", Comments: []Comment{{ID: 10, Number: 1, User: owner}}})
	cs := []Comment{{ID: 12, Number: 2, User: "x", Body: "b"}, {ID: 10, Number: 1, User: owner}, {ID: 11, Number: 1, User: owner, Body: "a\nb"}}
	next, ev := Reduce(st, Snapshot{At: "t1", Comments: cs})
	if len(ev) != 2 || ev[0].To != "11" || !ev[0].Owner || ev[1].To != "12" || ev[1].Owner {
		t.Fatalf("got %+v", ev)
	}
	if ev[0].Text != "#1 boss: a b" {
		t.Fatalf("text %q", ev[0].Text)
	}
	_, ev = Reduce(next, Snapshot{At: "t2", Comments: cs})
	if len(ev) != 0 {
		t.Fatalf("comments reported twice: %+v", ev)
	}
}

func TestCommentCap(t *testing.T) {
	st := base(t, Snapshot{At: "t0"})
	for i := 0; i < maxCommentIDs+10; i++ {
		st.CommentIDs = append(st.CommentIDs, int64(1000+i))
	}
	next, ev := Reduce(st, Snapshot{At: "t1", Comments: []Comment{{ID: 5, User: "x"}, {ID: 999999, User: "x"}}})
	if len(next.CommentIDs) != maxCommentIDs || len(ev) != 1 || ev[0].To != "999999" {
		t.Fatalf("cap: len=%d ev=%+v", len(next.CommentIDs), ev)
	}
}

func TestDuplicateInSnapshotIsAnomaly(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr()}})
	_, got := step(t, st, Snapshot{PRs: []ObservedPR{pr(), pr()}})
	if !reflect.DeepEqual(got, []string{"anomaly:>"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAggregateChecks(t *testing.T) {
	cases := []struct {
		in   []Check
		want string
	}{
		{nil, ChecksNone},
		{[]Check{{Status: "completed", Conclusion: "skipped"}, {Status: "completed", Conclusion: "neutral"}}, ChecksSuccess},
		{[]Check{{Status: "completed", Conclusion: "success"}, {Status: "queued"}}, ChecksPending},
		{[]Check{{Status: "queued"}, {Name: "a", Status: "completed", Conclusion: "cancelled"}}, ChecksFailure},
		{[]Check{{Status: "completed", Conclusion: "weird"}}, ChecksPending},
		{[]Check{{Status: "completed", Conclusion: "error"}}, ChecksFailure},
	}
	for i, c := range cases {
		if got, _ := aggregateChecks(c.in); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

// TestDeterministicReplay: the same inputs in any order of list items give byte-identical events and state.
func TestDeterministicReplay(t *testing.T) {
	s0 := Snapshot{At: "t0", PRs: []ObservedPR{pr()}, Issues: []ObservedIssue{{Number: 1, State: "open"}}}
	s1 := Snapshot{At: "t1",
		PRs:      []ObservedPR{pr(red, func(p *ObservedPR) { p.Labels = []string{"b", "a"} }), {Number: 2, State: "open", HeadSHA: sha2}},
		Issues:   []ObservedIssue{{Number: 1, State: "closed", Labels: []string{"z", "y"}}, {Number: 4, State: "open"}},
		Comments: []Comment{{ID: 3, User: owner}, {ID: 2, User: "x"}}}
	rev := s1
	rev.PRs = []ObservedPR{s1.PRs[1], s1.PRs[0]}
	rev.Issues = []ObservedIssue{s1.Issues[1], s1.Issues[0]}
	rev.Comments = []Comment{s1.Comments[1], s1.Comments[0]}

	run := func(s Snapshot) ([]byte, []byte) {
		st, _ := Reduce(NewState("o/r", owner), s0)
		next, ev := Reduce(st, s)
		a, _ := json.Marshal(next)
		b, _ := json.Marshal(ev)
		return a, b
	}
	a1, e1 := run(s1)
	a2, e2 := run(rev)
	a3, e3 := run(s1)
	if !bytes.Equal(a1, a2) || !bytes.Equal(e1, e2) || !bytes.Equal(a1, a3) || !bytes.Equal(e1, e3) {
		t.Fatalf("not deterministic:\n%s\n%s\n%s\n%s", e1, e2, a1, a2)
	}
}

func TestReduceDoesNotMutateInput(t *testing.T) {
	st := base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr()}})
	before, _ := json.Marshal(st)
	Reduce(st, Snapshot{At: "t1", PRs: []ObservedPR{pr(red)}, Comments: []Comment{{ID: 1}}})
	after, _ := json.Marshal(st)
	if !bytes.Equal(before, after) {
		t.Fatal("Reduce mutated its input state")
	}
}

func TestStoreRoundTripAndGuards(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	st, err := loadState(p, "o/r", owner)
	if err != nil || st.Cursor != "" {
		t.Fatalf("missing file should give a fresh state: %v %+v", err, st)
	}
	st = base(t, Snapshot{At: "t0", PRs: []ObservedPR{pr(green)}})
	if err := saveState(p, st); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(p, "o/r", owner)
	if err != nil || !reflect.DeepEqual(got, st) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, got, st)
	}
	if _, err := loadState(p, "other/r", owner); err == nil {
		t.Fatal("state of another repo accepted")
	}
	os.WriteFile(p, []byte(`{"version":99}`), 0o600)
	if _, err := loadState(p, "o/r", owner); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("wrong schema accepted: %v", err)
	}
}

func TestCLIReplay(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	write := func(name string, s Snapshot) string {
		p := filepath.Join(dir, name)
		b, _ := json.Marshal(s)
		os.WriteFile(p, b, 0o600)
		return p
	}
	s0 := write("0.json", Snapshot{At: "t0", PRs: []ObservedPR{pr()}})
	s1 := write("1.json", Snapshot{At: "t1", PRs: []ObservedPR{pr(green)}})
	args := []string{"-repo", "o/r", "-owner", owner, "-state", state}
	var out, errb bytes.Buffer
	if rc := run(append(args, "-replay", s0), &out, &errb); rc != 0 || out.Len() != 0 || !strings.Contains(errb.String(), "baseline") {
		t.Fatalf("baseline rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}
	out.Reset()
	if rc := run(append(args, "-replay", s1), &out, &errb); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	var e Event
	if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Kind != EvPRChecks || e.To != ChecksSuccess {
		t.Fatalf("event %q: %v", out.String(), err)
	}
	out.Reset()
	if rc := run(append(args, "-status"), &out, &errb); rc != 0 || !strings.Contains(out.String(), "checks=success") {
		t.Fatalf("status: %q", out.String())
	}
	if rc := run([]string{"-state", state}, &out, &errb); rc != 2 {
		t.Fatalf("missing flags: rc=%d", rc)
	}
}
