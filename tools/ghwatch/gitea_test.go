package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// #241 P8: the Gitea fetcher fills the same Snapshot; Reduce then reports owner reviews, checks and comments.
func stubGitea(t *testing.T, review string) *httptest.Server {
	sha := "0123456789abcdef0123456789abcdef01234567"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token gt-tok" {
			t.Errorf("auth %q on %s", got, r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "" {
			t.Errorf("GitHub paging on Gitea: %s", r.URL)
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/o/r")
		switch {
		case p == "/issues" && r.URL.Query().Get("type") == "pulls":
			io.WriteString(w, `[{"number":7}]`)
		case p == "/issues" && r.URL.Query().Get("labels") == "needs-decision":
			io.WriteString(w, `[{"number":3,"title":"Q","state":"open","labels":[{"name":"needs-decision"}]}]`)
		case p == "/issues":
			io.WriteString(w, `[{"number":3,"title":"Q","state":"open","labels":[{"name":"needs-decision"}],"assignees":[{"login":"alice"}]}]`)
		case p == "/issues/3/comments":
			io.WriteString(w, `[{"id":1,"user":{"login":"alice"},"body":"🤖 AI agent: which?","created_at":"2026-01-01T11:00:00+01:00"},
				{"id":2,"user":{"login":"boss"},"body":"B","created_at":"2026-01-01T11:30:00+01:00"}]`)
		case p == "/pulls":
			io.WriteString(w, `[{"number":7}]`)
		case p == "/pulls/7":
			io.WriteString(w, `{"number":7,"title":"Fix","state":"open","mergeable":true,"user":{"login":"alice"},
				"labels":[{"name":"agent:alice"}],"head":{"sha":"`+sha+`","ref":"agent/alice/issue-3"},"base":{"ref":"main"}}`)
		case p == "/pulls/7/reviews":
			io.WriteString(w, review)
		case p == "/pulls/7/reviews/41/comments":
			io.WriteString(w, `[{"id":900,"user":{"login":"boss"},"body":"rename","path":"a.go","created_at":"2026-01-01T12:00:00+01:00"}]`)
		case p == "/commits/"+sha+"/status":
			io.WriteString(w, `{"statuses":[{"context":"ci/test","status":"failure"},{"context":"lint","state":"success"}]}`)
		case p == "/issues/comments":
			io.WriteString(w, `[{"id":2,"user":{"login":"boss"},"body":"B","issue_url":"http://g/o/r/issues/3"}]`)
		case p == "/actions/runs":
			http.NotFound(w, r) // an older Gitea: no runs, not an error
		case p == "/releases":
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected call %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGiteaFetch(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	srv := stubGitea(t, `[{"id":40,"user":{"login":"alice"},"state":"REQUEST_REVIEW"},
		{"id":41,"user":{"login":"boss"},"state":"REQUEST_CHANGES","commit_id":"`+sha+`","comments_count":1},
		{"id":42,"user":{"login":"boss"},"state":"PENDING"}]`)
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	t.Setenv("GITEA_TOKEN", "gt-tok")
	args := []string{"-forge", "gitea", "-repo", "o/r", "-owner", owner, "-state", state, "-api", srv.URL + "/api/v1", "-format", "text"}
	var out, errb bytes.Buffer
	if rc := run(append(args, "-dump-snapshot", filepath.Join(dir, "snap.json")), &out, &errb); rc != 0 {
		t.Fatalf("baseline rc=%d %s", rc, errb.String())
	}
	st, err := loadState(state, "o/r", owner)
	if err != nil {
		t.Fatal(err)
	}
	pr := st.PRs["7"]
	if pr.Review != ReviewChangesRequested || pr.Checks != ChecksFailure || pr.Mergeable != MergeClean || pr.Lifecycle != PROpen {
		t.Fatalf("PR state: %+v", pr)
	}
	if len(pr.ReviewIDs) != 1 || pr.ReviewIDs[0] != 41 {
		t.Fatalf("only the submitted review counts: %v", pr.ReviewIDs)
	}
	if st.Issues["3"].Decision != DecisionAnswered {
		t.Fatalf("decision: %+v (offsets must compare as times)", st.Issues["3"])
	}
	if len(st.CommentIDs) != 2 {
		t.Fatalf("conversation and inline comments: %v", st.CommentIDs)
	}
}

func TestGiteaReviewStates(t *testing.T) {
	for in, want := range map[gtReview]string{
		{State: "APPROVED"}: "APPROVED", {State: "REQUEST_CHANGES"}: "CHANGES_REQUESTED", {State: "COMMENT"}: "COMMENTED",
		{State: "APPROVED", Dismissed: true}: "DISMISSED", {State: "PENDING"}: "", {State: "REQUEST_REVIEW"}: "",
	} {
		if got := gtReviewState(in); got != want {
			t.Errorf("%+v: %q, want %q", in, got, want)
		}
	}
	if gtTime("2026-01-01T11:00:00+01:00") != "2026-01-01T10:00:00Z" {
		t.Error("gtTime")
	}
}

func TestForgeFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"-forge", "bitbucket", "-repo", "o/r", "-owner", "x", "-state", filepath.Join(t.TempDir(), "s")}, &out, &errb); rc != 2 {
		t.Fatalf("unknown forge: rc=%d", rc)
	}
}
