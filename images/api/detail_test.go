package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// detailEnv: alice on owner/x (wendy writes there, rita reads), bob on owner/y (rita writes there).
func detailEnv(t *testing.T) (http.Handler, *writer) {
	t.Helper()
	srv := fakeGiteaPerms(t, map[string]string{"owner/x/wendy": "write", "owner/x/rita": "read", "owner/y/rita": "write"})
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList", approvalsGVR: "ApprovalList"},
		agentObj("alice", "owner/x", true), agentObj("bob", "owner/y", false),
		approvalObj("alice", "a1", "owner/x", time.Now()), approvalObj("bob", "b1", "owner/y", time.Now()))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-alice-0", Namespace: "coding-agents",
		Labels: map[string]string{"app": "coding-agent", "agent": "alice"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true, RestartCount: 2}}}}
	guides := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: guidesCM, Namespace: "coding-agents"},
		Data: map[string]string{"alice": "# agentdev harness guide\nalice", "bob": "bob guide"}}
	usage := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageCM, Namespace: "coding-agents"},
		Data: map[string]string{"pauses.json": `{"alice":{"until":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `,"reason":"weekly"}}`}}
	k := &kubeAgents{c: fake.NewSimpleClientset(pod, guides, usage), d: d, ns: "coding-agents"}
	wr := &writer{store: k, perms: newGiteaPerms(srv, "svc"), adminGroup: "agentdev-admins", usage: k}
	return routes(idVerify, k, newMemStore(), wr), wr
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAgentDetailVisibility(t *testing.T) {
	h, _ := detailEnv(t)
	for _, c := range []struct {
		token, name string
		code        int
	}{
		{"writer", "alice", 200}, // write on owner/x
		{"writer", "bob", 404},   // another project: not found, not 403
		{"reader", "alice", 404}, // read only on owner/x
		{"reader", "bob", 200},
		{"group", "alice", 200}, // admin group
		{"group", "bob", 200},
		{"nobody", "alice", 404},
		{"group", "nope", 404},
		{"group", "Bad_Name", 400},
		{"", "alice", 401},
	} {
		if w := do(t, h, "/api/agents/"+c.name, c.token); w.Code != c.code {
			t.Errorf("%s %s: %d, want %d: %s", c.token, c.name, w.Code, c.code, w.Body)
		}
	}
}

func TestAgentDetailContent(t *testing.T) {
	h, _ := detailEnv(t)
	w := do(t, h, "/api/agents/alice", "writer")
	var d struct {
		Name       string        `json:"name"`
		Paused     bool          `json:"paused"`
		Pods       []podView     `json:"pods"`
		WaitingOn  []waitingItem `json:"waitingOn"`
		RecentJobs []any         `json:"recentJobs"`
		Guide      string        `json:"guide"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Name != "alice" || !d.Paused || len(d.Pods) != 1 || !d.Pods[0].Ready || d.Pods[0].Restarts != 2 {
		t.Fatalf("detail: %s", w.Body)
	}
	if !strings.Contains(d.Guide, "harness guide") || strings.Contains(d.Guide, "bob") {
		t.Fatalf("guide: %q", d.Guide)
	}
	kinds := map[string]waitingItem{}
	for _, i := range d.WaitingOn {
		kinds[i.Kind] = i
	}
	if _, ok := kinds["paused"]; !ok {
		t.Fatalf("waiting on: no paused: %+v", d.WaitingOn)
	}
	if a, ok := kinds["approval"]; !ok || a.Repo != "owner/x" {
		t.Fatalf("waiting on: approval: %+v", d.WaitingOn)
	}
	if u, ok := kinds["usage-pause"]; !ok || u.Reason != "weekly" {
		t.Fatalf("waiting on: usage: %+v", d.WaitingOn)
	}
	if len(d.WaitingOn) != 3 || d.RecentJobs == nil {
		t.Fatalf("waiting on (bob's approval must not show): %s", w.Body)
	}
}

type fakePlatform struct {
	deploys map[string][3]int32 // ns/name -> ready, wanted, absent(1)
	phases  map[string]int
	err     error
}

func (f fakePlatform) DeploymentReady(_ context.Context, ns, name string) (int32, int32, bool, error) {
	if f.err != nil {
		return 0, 0, false, f.err
	}
	d, ok := f.deploys[ns+"/"+name]
	if !ok {
		return 0, 0, true, nil
	}
	return d[0], d[1], false, nil
}

func (f fakePlatform) AgentPodPhases(context.Context) (map[string]int, error) { return f.phases, f.err }

func TestPlatformAdminsOnly(t *testing.T) {
	h, wr := detailEnv(t)
	wr.platform = newPlatformChecker(fakePlatform{}, nil, "coding-agents", "agentdev-platform")
	h = routes(idVerify, wr.store.(*kubeAgents), newMemStore(), wr)
	for tok, code := range map[string]int{"writer": 403, "admin": 403, "reader": 403, "nobody": 403, "": 401, "group": 200} {
		if w := do(t, h, "/api/platform", tok); w.Code != code {
			t.Errorf("%q: %d, want %d", tok, w.Code, code)
		}
	}
}

func TestPlatformComponents(t *testing.T) {
	src := fakePlatform{deploys: map[string][3]int32{
		"coding-agents/webhook-router":        {1, 1},
		"agentdev-platform/agentdev-operator": {0, 1},
		"agentdev-platform/authentik-server":  {1, 2},
		"coding-agents/egress-proxy":          {1, 1},
	}, phases: map[string]int{"Running": 3, "Pending": 1}}
	p := newPlatformChecker(src, func(context.Context) error { return errors.New("401") }, "coding-agents", "agentdev-platform")
	p.version = "1.1.0"
	v := p.check(context.Background())
	got := map[string]string{}
	for _, c := range v.Components {
		got[c.Name] = c.Status
		if c.Command == "" {
			t.Errorf("%s: no command", c.Name)
		}
	}
	want := map[string]string{"api": "ok", "router": "ok", "operator": "down", "claude-proxy": "absent", "authentik": "degraded",
		"forge-token": "down", "egress-proxy": "ok", "agent-pool": "degraded"}
	for k, s := range want {
		if got[k] != s {
			t.Errorf("%s: %s, want %s", k, got[k], s)
		}
	}
	if v.Version != "1.1.0" || len(v.Components) != 8 {
		t.Fatalf("%+v", v)
	}
	// no credential: absent; Kubernetes error: unknown, never ok
	p = newPlatformChecker(fakePlatform{err: errors.New("forbidden")}, nil, "coding-agents", "agentdev-platform")
	for _, c := range p.check(context.Background()).Components {
		if c.Name == "forge-token" && c.Status != "absent" || c.Name == "router" && c.Status != "unknown" {
			t.Errorf("%s: %s", c.Name, c.Status)
		}
	}
	if imageTag("ghcr.io/a/agentdev-api:1.1.0") != "1.1.0" || imageTag("localhost:5000/api") != "" || imageTag("x:1.0@sha256:ab") != "1.0" {
		t.Error("imageTag")
	}
}

// the real kube implementation reads Deployments by name and counts agent pods
func TestKubePlatformSource(t *testing.T) {
	two := int32(2)
	k := &kubeAgents{c: fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "webhook-router", Namespace: "coding-agents"},
			Spec: appsv1.DeploymentSpec{Replicas: &two}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "coding-agents", Labels: map[string]string{"app": "coding-agent"}},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}), ns: "coding-agents"}
	r, w, absent, err := k.DeploymentReady(context.Background(), "coding-agents", "webhook-router")
	if r != 1 || w != 2 || absent || err != nil {
		t.Fatal(r, w, absent, err)
	}
	if _, _, absent, _ := k.DeploymentReady(context.Background(), "coding-agents", "claude-proxy"); !absent {
		t.Fatal("absent")
	}
	if ph, _ := k.AgentPodPhases(context.Background()); ph["Pending"] != 1 {
		t.Fatal(ph)
	}
}

// fakeGiteaPerms answers the collaborator permission endpoint from "owner/repo/user" -> level; token "svc".
func fakeGiteaPerms(t *testing.T, perms map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/permission")
		p = strings.Replace(p, "/collaborators/", "/", 1)
		v, ok := perms[p]
		if r.Header.Get("Authorization") != "token svc" || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"permission": v})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// #380: GET /api/agents lists only the agents the caller may see, with and without ?repo= (R35).
func TestAgentListVisibility(t *testing.T) {
	h, _ := detailEnv(t)
	names := func(path, token string) string {
		w := do(t, h, path, token)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", token, path, w.Code, w.Body)
		}
		var b struct {
			Agents []agent `json:"agents"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		var n []string
		for _, a := range b.Agents {
			n = append(n, a.Name)
		}
		return strings.Join(n, ",")
	}
	for _, c := range []struct{ path, token, want string }{
		{"/api/agents", "writer", "alice"},         // project A only
		{"/api/agents?repo=owner/y", "writer", ""}, // project B: nothing
		{"/api/agents?repo=owner/x", "writer", "alice"},
		{"/api/agents", "reader", "bob"}, // read on owner/x is not enough
		{"/api/agents?repo=owner/x", "reader", ""},
		{"/api/agents", "nobody", ""},
		{"/api/agents", "group", "alice,bob"}, // admin
		{"/api/agents?repo=owner/y", "group", "bob"},
	} {
		if got := names(c.path, c.token); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.token, c.path, got, c.want)
		}
	}
}
