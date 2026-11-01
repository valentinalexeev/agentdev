package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func approvalObj(agent, id, repo string, created time.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentdev.dev/v1alpha1", "kind": "Approval",
		"metadata": map[string]any{"name": agent + "-" + id, "namespace": "coding-agents"},
		"spec": map[string]any{"id": id, "agent": agent, "repo": repo, "forge": "gitea", "job": "issue-3", "source": "claude",
			"tool": "Bash", "input": `{"command":"npm i"}`, "contentHash": strings.Repeat("ab", 32),
			"requesterId": int64(99), "secretHash": secretHashOf(testSecret),
			"created": created.Format(time.RFC3339), "expires": created.Add(30 * time.Minute).Format(time.RFC3339)},
	}}
}

type fakeComments struct {
	list    []forgeComment
	ids     map[string]int64
	err     error
	closed  bool
	selfErr error
}

func (f *fakeComments) IssueState(context.Context, string, int) (string, error) {
	if f.closed {
		return "closed", nil
	}
	return "open", nil
}

type fakePods map[string]bool

func (p fakePods) PodExists(_ context.Context, uid string) (bool, error) { return p[uid], nil }

func (f *fakeComments) Comments(context.Context, string, int, time.Time) ([]forgeComment, error) {
	return f.list, f.err
}
func (f *fakeComments) Self(context.Context) (int64, error) {
	if f.selfErr != nil {
		return 0, f.selfErr
	}
	return 15, nil
}

func (f *fakeComments) UserID(_ context.Context, login string) (int64, error) {
	if id, ok := f.ids[login]; ok {
		return id, nil
	}
	return 0, errForgeUnavailable
}

type permMap map[string]perm

func (p permMap) Permission(_ context.Context, _, _, user string) (perm, error) { return p[user], nil }

type agentsFixed []agent

func (a agentsFixed) List(context.Context) ([]agent, error) { return a, nil }

func approvalEnv(t *testing.T, objs ...runtime.Object) (*kubeAgents, *strings.Builder) {
	t.Helper()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList", approvalsGVR: "ApprovalList"}, objs...)
	buf := &strings.Builder{}
	old := auditOut
	auditOut = buf
	t.Cleanup(func() { auditOut = old })
	return &kubeAgents{c: fake.NewSimpleClientset(), d: d, ns: "coding-agents"}, buf
}

func status(t *testing.T, k *kubeAgents, name string) approval {
	t.Helper()
	u, err := k.GetApproval(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return fromApprovalObject(u)
}

const aid = "0123456789abcdef"

var testSecret = strings.Repeat("5", 64)

func secretHashOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestDeciderRules(t *testing.T) {
	cases := []struct {
		name    string
		c       forgeComment
		state   string
		scope   string
		refused bool
	}{
		{"write user approves once", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "allowed", "once", false},
		{"approve for the job", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "ok\n/approve-job " + aid, Created: t0.Add(time.Minute)}, "allowed", "job", false},
		{"deny", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/deny " + aid, Created: t0.Add(time.Minute)}, "denied", "", false},
		{"agent account", forgeComment{ID: 1, UserID: 55, Login: "wendy2", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"requester with another login", forgeComment{ID: 1, UserID: 99, Login: "carol-bot", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"other agent", forgeComment{ID: 1, UserID: 77, Login: "dave", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"platform account", forgeComment{ID: 1, UserID: 14, Login: "Router-Bot", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"API token account", forgeComment{ID: 1, UserID: 15, Login: "svc", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"bot account", forgeComment{ID: 1, UserID: 11, Login: "wendy", Bot: true, Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"read only", forgeComment{ID: 1, UserID: 12, Login: "rita", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"older than request", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(-time.Minute)}, "pending", "", true},
		{"edited", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute), Updated: t0.Add(2 * time.Minute)}, "pending", "", true},
		{"other id", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve fedcba9876543210", Created: t0.Add(time.Minute)}, "pending", "", false},
		{"inside a code fence", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "plan:\n```\n/approve " + aid + "\n```", Created: t0.Add(time.Minute)}, "pending", "", false},
		{"quoted line", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "> /approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", false},
		{"mid-line mention", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "do not /approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
			// carol's forge login differs from her name: her id is only known from spec.requesterId
			fc := &fakeComments{list: []forgeComment{c.c}, ids: map[string]int64{"carol": 55, "dave": 77}}
			d := newDecider(k, agentsFixed{{Name: "carol"}, {Name: "dave"}}, fc,
				permMap{"wendy": permWrite, "rita": permRead, "carol-bot": permWrite, "dave": permWrite, "Router-Bot": permWrite, "svc": permAdmin})
			d.denyLogins["router-bot"] = true
			d.now = func() time.Time { return t0.Add(5 * time.Minute) }
			if err := d.pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			a := status(t, k, "carol-"+aid)
			if a.State != c.state || a.Scope != c.scope {
				t.Fatalf("state %s/%s, want %s/%s", a.State, a.Scope, c.state, c.scope)
			}
			if c.state != "pending" && (a.DecidedBy != "wendy" || a.DecidedHash != strings.Repeat("ab", 32) || a.CommentID != 1) {
				t.Fatalf("decision record: %+v", a)
			}
			if got := strings.Contains(buf.String(), `"result":"denied"`); got != c.refused {
				t.Fatalf("refused audit %v, want %v: %s", got, c.refused, buf)
			}
			if c.state != "pending" && !strings.Contains(buf.String(), `"action":"approval-`+c.state+`"`) {
				t.Fatalf("no decision audit: %s", buf)
			}
		})
	}
}

func TestDeciderFirstWinsAndExpiry(t *testing.T) {
	k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	fc := &fakeComments{ids: map[string]int64{"carol": 99}, list: []forgeComment{
		{ID: 2, UserID: 10, Login: "wendy", Body: "/deny " + aid, Created: t0.Add(2 * time.Minute)},
		{ID: 1, UserID: 13, Login: "adam", Body: "/approve " + aid, Created: t0.Add(time.Minute)},
	}}
	d := newDecider(k, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite, "adam": permAdmin})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "adam" {
		t.Fatalf("first comment must win: %+v", a)
	}
	// a decided request is not looked at again
	d.now = func() time.Time { return t0.Add(time.Hour) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "allowed" {
		t.Fatalf("decided request changed: %+v", a)
	}

	// no expiry: a pending request long after its wait time stays pending and decidable (owner on #107)
	k2, _ := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	fc2 := &fakeComments{ids: map[string]int64{"carol": 99}}
	d2 := newDecider(k2, agentsFixed{{Name: "carol"}}, fc2, permMap{"wendy": permWrite})
	d2.now = func() time.Time { return t0.Add(3 * time.Hour) }
	_ = d2.pass(context.Background())
	if a := status(t, k2, "carol-"+aid); a.State != "pending" {
		t.Fatalf("timed out request changed: %+v", a)
	}
	fc2.list = []forgeComment{{ID: 5, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(2 * time.Hour)}}
	_ = d2.pass(context.Background())
	if a := status(t, k2, "carol-"+aid); a.State != "allowed" {
		t.Fatalf("late decision on the open request: %+v", a)
	}
	_ = buf
}

func TestDeciderFailsClosed(t *testing.T) {
	k, _ := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	c := forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}
	// an agent account that cannot be resolved is skipped; the requester is still refused by its id
	self := forgeComment{ID: 2, UserID: 99, Login: "carol", Body: "/approve " + aid, Created: t0.Add(30 * time.Second)}
	d := newDecider(k, agentsFixed{{Name: "carol"}, {Name: "ghost"}}, &fakeComments{list: []forgeComment{self, c}}, permMap{"wendy": permWrite, "carol": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	if err := d.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "wendy" {
		t.Fatalf("want wendy's decision, not the requester's: %+v", a)
	}
	// seen is pruned after the request expired
	d.now = func() time.Time { return t0.Add(lateWindow + time.Hour) }
	d.pruneSeen()
	if len(d.seen) != 0 {
		t.Fatalf("seen not pruned: %v", d.seen)
	}
}

// The API's own Gitea account cannot be resolved: no decision in that pass (it could be the commenter), then it applies.
func TestDeciderSelfUnknownFailsClosed(t *testing.T) {
	k, _ := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	fc := &fakeComments{selfErr: errForgeUnavailable, list: []forgeComment{
		{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}}}
	d := newDecider(k, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	if err := d.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("own account unknown: state %s, want pending", a.State)
	}
	fc.selfErr = nil
	if err := d.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := status(t, k, "carol-"+aid); a.State != "allowed" || a.DecidedBy != "wendy" {
		t.Fatalf("own account known: %+v", a)
	}
}

type conflictStore struct{ *kubeAgents }

func (conflictStore) UpdateApprovalStatus(context.Context, *unstructured.Unstructured) error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "approvals"}, "x", nil)
}

func TestDeciderConflict(t *testing.T) {
	k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	fc := &fakeComments{ids: map[string]int64{"carol": 99}, list: []forgeComment{{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}}}
	d := newDecider(conflictStore{k}, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if !strings.Contains(buf.String(), "another decision was first") {
		t.Fatalf("conflict not audited: %s", buf)
	}
}

func TestInternalDecision(t *testing.T) {
	obj := approvalObj("carol", aid, "owner/x", t0)
	obj.Object["status"] = map[string]any{"state": "allowed", "scope": "job", "decidedBy": "wendy", "contentHash": "h"}
	k, _ := approvalEnv(t, obj)
	h := internalDecision(k)
	mux := http.NewServeMux()
	mux.Handle("GET /internal/approvals/{agent}/{id}", h)
	secret := testSecret
	get := func(p string) (int, map[string]string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("X-Approval-Secret", secret)
		mux.ServeHTTP(rec, req)
		var m map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	if code, m := get("/internal/approvals/carol/" + aid); code != 200 || m["state"] != "allowed" || m["scope"] != "job" || m["contentHash"] != "h" {
		t.Fatalf("%d %v", code, m)
	}
	if _, m := get("/internal/approvals/carol/fedcba9876543210"); m["state"] != "pending" {
		t.Fatalf("unknown: %v", m)
	}
	secret = strings.Repeat("6", 64) // another pod without the request's secret
	if code, m := get("/internal/approvals/carol/" + aid); code != 404 || m["state"] != "" {
		t.Fatalf("wrong secret: %d %v", code, m)
	}
	if code, _ := get("/internal/approvals/carol/..%2f"); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
}

func TestApprovalReadAPI(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	k, _ := approvalEnv(t, agentObj("alice", "owner/x", false),
		approvalObj("alice", aid, "owner/x", t0), approvalObj("zed", "fedcba9876543210", "other/y", t0))
	wr := &writer{store: k, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"}
	h := routes(idVerify, k, nil, wr)
	list := func(token, q string) []approval {
		r := httptest.NewRequest("GET", "/api/approvals"+q, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out struct{ Approvals []approval }
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Approvals
	}
	if got := list("writer", ""); len(got) != 1 || got[0].Repo != "owner/x" || !got[0].Untrusted {
		t.Fatalf("writer sees: %+v", got)
	}
	if got := list("reader", ""); len(got) != 0 {
		t.Fatalf("reader sees: %+v", got)
	}
	if got := list("group", "?state=pending"); len(got) != 2 {
		t.Fatalf("admin group sees: %+v", got)
	}
	if got := list("group", "?state=decided"); len(got) != 0 {
		t.Fatalf("decided filter: %+v", got)
	}
	r := httptest.NewRequest("GET", "/api/approvals?state=bogus", nil)
	r.Header.Set("Authorization", "Bearer group")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("bad state: %d", w.Code)
	}
	for tok, want := range map[string]int{"writer": 404, "group": 200} {
		r := httptest.NewRequest("GET", "/api/approvals/zed-fedcba9876543210", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s get: %d", tok, w.Code)
		}
	}
}

func TestDeciderVoid(t *testing.T) {
	// the issue or PR is closed
	k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	fc := &fakeComments{ids: map[string]int64{"carol": 99}, closed: true,
		list: []forgeComment{{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}}}
	d := newDecider(k, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "void" || !strings.Contains(a.Reason, "closed") {
		t.Fatalf("closed issue: %+v", a)
	}
	if !strings.Contains(buf.String(), `"action":"approval-void"`) {
		t.Fatalf("void not audited: %s", buf)
	}
	// a decision after the void is ignored and audited
	fc.list = append(fc.list, forgeComment{ID: 2, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(10 * time.Minute)})
	d.now = func() time.Time { return t0.Add(11 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "carol-"+aid); a.State != "void" {
		t.Fatalf("void changed: %+v", a)
	}
	if !strings.Contains(buf.String(), "a later decision has no effect") {
		t.Fatalf("late decision not audited: %s", buf)
	}

	// the pod that asked is gone
	obj := approvalObj("carol", "fedcba9876543210", "owner/x", t0)
	obj.SetLabels(map[string]string{"agentdev/pod-uid": "uid-1"})
	k2, _ := approvalEnv(t, obj)
	d2 := newDecider(k2, agentsFixed{{Name: "carol"}}, &fakeComments{ids: map[string]int64{"carol": 99}}, permMap{})
	d2.pods = fakePods{"uid-2": true}
	_ = d2.pass(context.Background())
	if a := status(t, k2, "carol-fedcba9876543210"); a.State != "void" {
		t.Fatalf("gone pod: %+v", a)
	}
}

func TestInternalVoid(t *testing.T) {
	k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	mux := http.NewServeMux()
	mux.Handle("POST /internal/approvals/{agent}/{id}/void", internalVoid(k))
	post := func(secret string) int {
		req := httptest.NewRequest("POST", "/internal/approvals/carol/"+aid+"/void", strings.NewReader(`{"reason":"the job ended"}`))
		req.Header.Set("X-Approval-Secret", secret)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(strings.Repeat("6", 64)); code != 404 {
		t.Fatalf("wrong secret: %d", code)
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("voided by another pod: %+v", a)
	}
	if code := post(testSecret); code != 200 {
		t.Fatalf("void: %d", code)
	}
	if a := status(t, k, "carol-"+aid); a.State != "void" || a.Reason != "the job ended" {
		t.Fatalf("not void: %+v", a)
	}
	if !strings.Contains(buf.String(), `"action":"approval-void"`) {
		t.Fatalf("not audited: %s", buf)
	}
}

func TestCommandLines(t *testing.T) {
	body := "ok\n```\n/approve x\n```\n~~~~\n/deny y\n~~~~\n> /approve z\n    /approve w\n/approve-job v"
	if got := commandLines(body); got != "ok\n/approve-job v" {
		t.Fatalf("got %q", got)
	}
}

// An executor apply approval lives on a merged (closed) pull request: it is not void for that and stays decidable.
func TestDeciderExecutorOnClosedPR(t *testing.T) {
	obj := approvalObj("tf-prod", aid, "owner/x", t0)
	spec := obj.Object["spec"].(map[string]any)
	spec["source"], spec["job"], spec["tool"] = "executor", "pr-7", "apply"
	k, _ := approvalEnv(t, obj)
	fc := &fakeComments{ids: map[string]int64{}, closed: true,
		list: []forgeComment{{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}}}
	d := newDecider(k, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	_ = d.pass(context.Background())
	if a := status(t, k, "tf-prod-"+aid); a.State != "allowed" || a.Scope != "once" {
		t.Fatalf("executor approval on a closed PR: %+v", a)
	}
}
