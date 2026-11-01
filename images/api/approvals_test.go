package main

import (
	"context"
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
			"created": created.Format(time.RFC3339), "expires": created.Add(30 * time.Minute).Format(time.RFC3339)},
	}}
}

type fakeComments struct {
	list []forgeComment
	ids  map[string]int64
	err  error
}

func (f *fakeComments) Comments(context.Context, string, int, time.Time) ([]forgeComment, error) {
	return f.list, f.err
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
		{"agent account", forgeComment{ID: 1, UserID: 99, Login: "wendy2", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"bot account", forgeComment{ID: 1, UserID: 11, Login: "wendy", Bot: true, Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"read only", forgeComment{ID: 1, UserID: 12, Login: "rita", Body: "/approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", true},
		{"older than request", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(-time.Minute)}, "pending", "", true},
		{"edited", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute), Updated: t0.Add(2 * time.Minute)}, "pending", "", true},
		{"other id", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve fedcba9876543210", Created: t0.Add(time.Minute)}, "pending", "", false},
		{"mid-line mention", forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "do not /approve " + aid, Created: t0.Add(time.Minute)}, "pending", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, buf := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
			fc := &fakeComments{list: []forgeComment{c.c}, ids: map[string]int64{"carol": 99}}
			d := newDecider(k, agentsFixed{{Name: "carol"}}, fc, permMap{"wendy": permWrite, "rita": permRead})
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

	k2, buf2 := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	d2 := newDecider(k2, agentsFixed{{Name: "carol"}}, &fakeComments{ids: map[string]int64{"carol": 99}}, permMap{})
	d2.now = func() time.Time { return t0.Add(31 * time.Minute) }
	_ = d2.pass(context.Background())
	if a := status(t, k2, "carol-"+aid); a.State != "expired" {
		t.Fatalf("not expired: %+v", a)
	}
	if !strings.Contains(buf2.String(), `"action":"approval-expire"`) {
		t.Fatalf("no expiry audit: %s", buf2)
	}
	_ = buf
}

func TestDeciderFailsClosed(t *testing.T) {
	k, _ := approvalEnv(t, approvalObj("carol", aid, "owner/x", t0))
	c := forgeComment{ID: 1, UserID: 10, Login: "wendy", Body: "/approve " + aid, Created: t0.Add(time.Minute)}
	// agent account id cannot be resolved: no decision
	d := newDecider(k, agentsFixed{{Name: "carol"}}, &fakeComments{list: []forgeComment{c}}, permMap{"wendy": permWrite})
	d.now = func() time.Time { return t0.Add(5 * time.Minute) }
	if err := d.pass(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if a := status(t, k, "carol-"+aid); a.State != "pending" {
		t.Fatalf("decided without knowing agent accounts: %+v", a)
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
	get := func(p string) (int, map[string]string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
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
