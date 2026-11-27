package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const mergeHead = "1111111111111111111111111111111111111111"

// mergeForge is a fake GitHub for POST /merge (#578): one PR #20 of alice, its reviews, checks and files.
type mergeForge struct {
	mu       sync.Mutex
	pr       map[string]any
	reviews  []map[string]any
	perm     map[string]string // login -> permission
	runs     []map[string]any
	statuses []map[string]any
	files    []map[string]any
	mergeRC  int
	merged   []string // bodies of the merge calls
	audits   []string // bodies the API's internal listener got
}

func newMergeForge() *mergeForge {
	return &mergeForge{
		pr: map[string]any{"state": "open", "draft": false, "merged": false, "mergeable": true, "mergeable_state": "clean",
			"user": map[string]any{"login": "my-agentdev[bot]", "type": "Bot"},
			"head": map[string]any{"ref": "agent/alice/issue-12", "sha": mergeHead, "repo": map[string]any{"full_name": "octo-org/repo"}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "octo-org/repo"}}},
		reviews: []map[string]any{review("owner", "User", "APPROVED", mergeHead)},
		perm:    map[string]string{"owner": "admin", "reader": "read", "helper": "write"},
		runs:    []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}},
		files:   []map[string]any{{"filename": "main.go"}},
		mergeRC: 200,
	}
}

func review(login, typ, state, commit string) map[string]any {
	return map[string]any{"user": map[string]any{"login": login, "type": typ}, "state": state, "commit_id": commit}
}

func (f *mergeForge) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		switch {
		case p == "/repos/octo-org/repo/installation":
			w.Write(ghFixture(t, "installation.json"))
		case r.Method == "POST" && p == "/app/installations/51234567/access_tokens":
			w.WriteHeader(201)
			w.Write(ghFixture(t, "access_token.json"))
		case p == "/app":
			w.Write(ghFixture(t, "app.json"))
		case p == "/users/my-agentdev[bot]":
			w.Write(ghFixture(t, "bot_user.json"))
		case p == "/repos/octo-org/repo":
			js(map[string]any{"default_branch": "main"})
		case p == "/repos/octo-org/repo/pulls/20":
			js(f.pr)
		case p == "/repos/octo-org/repo/pulls/20/reviews":
			if r.URL.Query().Get("page") != "1" {
				js([]any{})
				return
			}
			js(f.reviews)
		case strings.HasPrefix(p, "/repos/octo-org/repo/collaborators/"):
			login := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/octo-org/repo/collaborators/"), "/permission")
			perm, ok := f.perm[login]
			if !ok {
				w.WriteHeader(404)
				return
			}
			js(map[string]any{"permission": perm})
		case p == "/repos/octo-org/repo/commits/"+mergeHead+"/check-runs":
			js(map[string]any{"total_count": len(f.runs), "check_runs": f.runs})
		case p == "/repos/octo-org/repo/commits/"+mergeHead+"/status":
			state := "pending"
			if len(f.statuses) > 0 {
				state = "success"
				for _, s := range f.statuses {
					if s["state"] != "success" {
						state = s["state"].(string)
					}
				}
			}
			js(map[string]any{"state": state, "total_count": len(f.statuses), "statuses": f.statuses})
		case p == "/repos/octo-org/repo/pulls/20/files":
			if r.URL.Query().Get("page") != "1" {
				js([]any{})
				return
			}
			js(f.files)
		case r.Method == "PUT" && p == "/repos/octo-org/repo/pulls/20/merge":
			b, _ := io.ReadAll(r.Body)
			f.merged = append(f.merged, string(b))
			w.WriteHeader(f.mergeRC)
			if f.mergeRC == 200 {
				js(map[string]any{"merged": true, "sha": "2222222222222222222222222222222222222222"})
			}
		case r.Method == "POST" && p == "/internal/merges/alice":
			b, _ := io.ReadAll(r.Body)
			f.audits = append(f.audits, string(b))
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(404)
		}
	})
}

// mergeServer: a sidecar with MERGE_MODE=gated on the fake GitHub; out collects the stdout lines.
func mergeServer(t *testing.T, f *mergeForge, mode string) (http.Handler, *bytes.Buffer) {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c, _ := ghConfig(t, srv.URL)
	c.MergeMode, c.APIURL = mode, srv.URL
	c.MergeApprovers = []string{"Owner"}
	c.Protected = []string{".github/workflows/"}
	d, err := newGH(c)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return ghNow.Add(-30 * time.Minute) }
	s := newServer(c)
	s.drv, s.rules = d, d.rules()
	var out bytes.Buffer
	old := eventOut
	eventOut = &out
	t.Cleanup(func() { eventOut = old })
	return s.routes(), &out
}

func postMerge(h http.Handler, pr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/merge?pr="+pr, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Agentdev-Job", "pr-"+pr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// eventRules returns the rules of the security events in out.
func eventRules(out *bytes.Buffer) []string {
	var rules []string
	for _, l := range lines(out.String()) {
		if js, ok := strings.CutPrefix(l, eventPrefix); ok {
			var e securityEvent
			_ = json.Unmarshal([]byte(js), &e)
			rules = append(rules, e.Rule+"/"+e.Severity)
		}
	}
	return rules
}

func TestMergeApprovedHead(t *testing.T) {
	f := newMergeForge()
	h, out := mergeServer(t, f, "gated")
	rec := postMerge(h, "20")
	if rec.Code != 200 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body)
	}
	if len(f.merged) != 1 || !strings.Contains(f.merged[0], `"sha":"`+mergeHead+`"`) || !strings.Contains(f.merged[0], `"merge_method":"merge"`) {
		t.Fatalf("merge call must pin the head and the method: %v", f.merged)
	}
	var res mergeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Approver != "owner" || res.SHA != mergeHead || len(res.Checks) != 1 {
		t.Fatalf("answer: %s", rec.Body)
	}
	if !strings.Contains(out.String(), auditPrefix) || !strings.Contains(out.String(), `"action":"agent-merge"`) {
		t.Errorf("no audit line: %s", out)
	}
	if len(f.audits) != 1 || !strings.Contains(f.audits[0], `"approver":"owner"`) {
		t.Errorf("audit to the API: %v", f.audits)
	}
	if r := eventRules(out); len(r) != 0 {
		t.Errorf("a merge is not a security event: %v", r)
	}
}

func TestMergeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(f *mergeForge)
		code int
		want string // part of the answer
		rule string // security event, "" for none
	}{
		"stale approval": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("owner", "User", "APPROVED", strings.Repeat("0", 40))}
		}, 403, "stale approval", "merge-refused/medium"},
		"bot approval": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("my-agentdev[bot]", "Bot", "APPROVED", mergeHead)}
		}, 403, "agent or a bot", "merge-refused/high"},
		"agent's own login": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("alice", "User", "APPROVED", mergeHead)}
		}, 403, "agent or a bot", "merge-refused/high"},
		"no write access": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("reader", "User", "APPROVED", mergeHead)}
		}, 403, "not an owner", "merge-refused/medium"},
		"approval by a non-owner with write access": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("helper", "User", "APPROVED", mergeHead)}
		}, 403, "not an owner", "merge-refused/medium"},
		"dismissed owner approval": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("owner", "User", "APPROVED", mergeHead), review("owner", "User", "DISMISSED", mergeHead)}
		}, 403, "dismissed", "merge-refused/medium"},
		"owner approved another commit, non-owner the head": {func(f *mergeForge) {
			f.reviews = []map[string]any{review("owner", "User", "APPROVED", strings.Repeat("0", 40)), review("helper", "User", "APPROVED", mergeHead)}
		}, 403, "stale approval", "merge-refused/medium"},
		"no reviews": {func(f *mergeForge) { f.reviews = nil }, 403, "no approval", "merge-refused/medium"},
		"pending status": {func(f *mergeForge) {
			f.statuses = []map[string]any{{"context": "ci/x", "state": "pending"}}
		}, 409, "ci/x is pending", ""},
		"cancelled check": {func(f *mergeForge) {
			f.runs = append(f.runs, map[string]any{"name": "e2e", "status": "completed", "conclusion": "cancelled"})
		}, 403, "e2e: cancelled", "merge-refused/medium"},
		"changes requested": {func(f *mergeForge) {
			f.reviews = append(f.reviews, review("helper", "User", "CHANGES_REQUESTED", mergeHead))
		}, 403, "requests changes", "merge-refused/medium"},
		"failing check": {func(f *mergeForge) {
			f.runs = append(f.runs, map[string]any{"name": "lint", "status": "completed", "conclusion": "failure"})
		}, 403, "lint: failure", "merge-refused/medium"},
		"pending check": {func(f *mergeForge) {
			f.runs = append(f.runs, map[string]any{"name": "e2e", "status": "in_progress"})
		}, 409, "e2e is in_progress", ""},
		"failing status": {func(f *mergeForge) {
			f.statuses = []map[string]any{{"context": "ci/x", "state": "failure"}}
		}, 403, "ci/x: failure", "merge-refused/medium"},
		"blocked by branch rules": {func(f *mergeForge) { f.pr["mergeable_state"] = "blocked" }, 409, "state blocked", ""},
		"behind":                  {func(f *mergeForge) { f.pr["mergeable_state"] = "behind" }, 409, "behind", ""},
		"protected path without approvals": {func(f *mergeForge) {
			f.files = append(f.files, map[string]any{"filename": ".github/workflows/ci.yml"})
		}, 403, "protected paths", "merge-refused/high"},
		"protected path renamed away": {func(f *mergeForge) {
			f.files = []map[string]any{{"filename": "x.yml", "previous_filename": ".github/workflows/ci.yml"}}
		}, 403, "protected paths", "merge-refused/high"},
		"another agent's branch": {func(f *mergeForge) {
			f.pr["head"].(map[string]any)["ref"] = "agent/bob/issue-9"
		}, 403, "not from a branch of alice", "merge-refused/high"},
		"a fork": {func(f *mergeForge) {
			f.pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/repo"}
		}, 403, "not from a branch of alice", "merge-refused/high"},
		"opened by someone else": {func(f *mergeForge) {
			f.pr["user"] = map[string]any{"login": "mallory", "type": "User"}
		}, 403, "not opened by the agent", "merge-refused/high"},
		"another base": {func(f *mergeForge) {
			f.pr["base"].(map[string]any)["ref"] = "release/1.0"
		}, 403, "not the default branch", "merge-refused/high"},
		"closed":                  {func(f *mergeForge) { f.pr["state"] = "closed" }, 403, "not open", "merge-refused/low"},
		"head moved at the merge": {func(f *mergeForge) { f.mergeRC = 409 }, 403, "moved", "merge-refused/medium"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeForge()
			tc.mut(f)
			h, out := mergeServer(t, f, "gated")
			rec := postMerge(h, "20")
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("got %d %q, want %d with %q", rec.Code, rec.Body, tc.code, tc.want)
			}
			if tc.mut != nil && name != "head moved at the merge" && len(f.merged) != 0 {
				t.Errorf("merge endpoint called: %v", f.merged)
			}
			if len(f.audits) != 0 || strings.Contains(out.String(), auditPrefix) {
				t.Errorf("audit entry for a refused merge")
			}
			got := eventRules(out)
			if tc.rule == "" && len(got) != 0 || tc.rule != "" && (len(got) != 1 || got[0] != tc.rule) {
				t.Errorf("events %v, want %q", got, tc.rule)
			}
		})
	}
}

// the latest deciding review of a person counts: an approval after a request for changes clears it
func TestMergeLatestReviewCounts(t *testing.T) {
	f := newMergeForge()
	f.reviews = []map[string]any{review("owner", "User", "CHANGES_REQUESTED", strings.Repeat("0", 40)),
		review("owner", "User", "COMMENTED", mergeHead), review("owner", "User", "APPROVED", mergeHead)}
	h, _ := mergeServer(t, f, "gated")
	if rec := postMerge(h, "20"); rec.Code != 200 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body)
	}
}

func TestMergeOffHasNoEndpoint(t *testing.T) {
	f := newMergeForge()
	h, _ := mergeServer(t, f, "off")
	if rec := postMerge(h, "20"); rec.Code != 404 {
		t.Fatalf("MERGE_MODE=off: %d, want 404 (no endpoint)", rec.Code)
	}
	if len(f.merged) != 0 {
		t.Fatal("merged with mode off")
	}
}

func TestMergeGiteaHasNoEndpoint(t *testing.T) {
	s := newServer(&Config{ForgeURL: "http://gitea", ForgeUser: "alice", Token: "t", Repos: []string{"o/r"}, MergeMode: "gated"})
	req := httptest.NewRequest("POST", "/merge?pr=1", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("gitea: %d, want 404", rec.Code)
	}
}

func TestMergeBadPR(t *testing.T) {
	h, _ := mergeServer(t, newMergeForge(), "gated")
	for _, pr := range []string{"", "0", "20x", "-1", "1;2"} {
		if rec := postMerge(h, pr); rec.Code != 400 {
			t.Errorf("pr=%q: %d", pr, rec.Code)
		}
	}
}

func TestMergeConfig(t *testing.T) {
	for _, tc := range []struct {
		mode, method string
		ok           bool
		approvers    string
	}{{"", "", true, ""}, {"off", "", true, ""}, {"gated", "", true, "owner"}, {"gated", "merge", true, "a, b"},
		{"on", "", false, "owner"}, {"gated", "squash", false, "owner"}, {"gated", "rebase", false, "owner"},
		{"off", "squash", false, ""},
		{"gated", "", false, ""}, {"gated", "", false, " , "}, {"gated", "", false, "bad login!"}} {
		env := map[string]string{"MERGE_MODE": tc.mode, "MERGE_METHOD": tc.method, "MERGE_APPROVERS": tc.approvers}
		_, _, err := mergeConfig(func(k, def string) string {
			if v := env[k]; v != "" {
				return v
			}
			return def
		})
		if (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// the caller cannot choose the merge method or the approval: the query and body are ignored beyond pr
func TestMergeCallerCannotChooseMethodOrApprover(t *testing.T) {
	f := newMergeForge()
	f.reviews = []map[string]any{review("helper", "User", "APPROVED", mergeHead)}
	h, _ := mergeServer(t, f, "gated")
	req := httptest.NewRequest("POST", "/merge?pr=20&merge_method=rebase&approver=helper&sha="+mergeHead,
		strings.NewReader(`{"merge_method":"rebase","approver":"helper","approved":true}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || len(f.merged) != 0 {
		t.Fatalf("forged approval: %d %s, merged %v", rec.Code, rec.Body, f.merged)
	}
	f.reviews = []map[string]any{review("owner", "User", "APPROVED", mergeHead)}
	req = httptest.NewRequest("POST", "/merge?pr=20&merge_method=rebase", strings.NewReader(`{"merge_method":"rebase"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(f.merged) != 1 || !strings.Contains(f.merged[0], `"merge_method":"merge"`) {
		t.Fatalf("the method is always merge: %d %v", rec.Code, f.merged)
	}
}

// a console chat pod never merges (#578, #571): refused with the chat event before any forge call, GitHub and Gitea
func TestMergeRefusedInChat(t *testing.T) {
	f := newMergeForge()
	var calls int
	inner := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/internal/") {
			calls++
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, _ := ghConfig(t, srv.URL)
	c.MergeMode, c.APIURL, c.MergeApprovers, c.ChatSession = "gated", srv.URL, []string{"Owner"}, testChatSession
	d, err := newGH(c)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return ghNow.Add(-30 * time.Minute) }
	s := newServer(c)
	s.drv, s.rules = d, d.rules()
	var out bytes.Buffer
	old := eventOut
	eventOut = &out
	t.Cleanup(func() { eventOut = old })
	rec := postMerge(s.routes(), "20")
	if rec.Code != 403 {
		t.Fatalf("github chat merge: %d %s, want 403", rec.Code, rec.Body)
	}
	if calls != 0 || len(f.merged) != 0 {
		t.Fatalf("github chat merge reached the forge: %d calls, merged %v", calls, f.merged)
	}
	if r := eventRules(&out); len(r) != 1 || r[0] != "chat-label-not-asked/medium" {
		t.Fatalf("events %v, want the chat refusal", r)
	}
	if _, err := s.mergePR(context.Background(), "pr-20", "20"); err == nil || calls != 0 {
		t.Fatalf("mergePR in a chat: %v, %d calls", err, calls)
	}

	// Gitea: no endpoint at all, and mergePR itself refuses before any call
	var gcalls int
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gcalls++; w.WriteHeader(500) }))
	t.Cleanup(gsrv.Close)
	gs := newServer(&Config{ForgeURL: gsrv.URL, ForgeUser: "alice", Token: "t", Repos: []string{"o/r"}, MergeMode: "gated", ChatSession: testChatSession})
	req := httptest.NewRequest("POST", "/merge?pr=1", nil)
	req.RemoteAddr = "127.0.0.1:1"
	grec := httptest.NewRecorder()
	gs.routes().ServeHTTP(grec, req)
	if grec.Code != 404 {
		t.Fatalf("gitea chat merge: %d, want 404", grec.Code)
	}
	var pe *policyError
	if _, err := gs.mergePR(context.Background(), "pr-1", "1"); !errors.As(err, &pe) || pe.rule != "chat-label-not-asked" {
		t.Fatalf("gitea mergePR in a chat: %v", err)
	}
	if gcalls != 0 {
		t.Fatalf("gitea chat merge reached the forge: %d calls", gcalls)
	}
}
