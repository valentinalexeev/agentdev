package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const ns = "coding-agents"

func setup(t *testing.T, objs ...client.Object) (*AgentReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&v1.Agent{}).Build()
	return &AgentReconciler{Client: c, D: Defaults{WorkerImage: "worker:dev", ForgeImage: "forge:dev", PullPolicy: "Never", ForgeURL: "http://forge",
		PollSeconds: "20", IdleSeconds: "900", Reviewer: "owner"}}, c
}

func agent(name string, mut func(*v1.Agent)) *v1.Agent {
	a := &v1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		Spec: v1.AgentSpec{Repo: "owner/repo", ClaudeSecret: "claude-shared"}}
	if mut != nil {
		mut(a)
	}
	return a
}

func reconcile(t *testing.T, r *AgentReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func getSTS(t *testing.T, c client.Client, name string) *appsv1.StatefulSet {
	t.Helper()
	var s appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-" + name}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func envOf(s *appsv1.StatefulSet, k string) string {
	for _, e := range s.Spec.Template.Spec.Containers[0].Env {
		if e.Name == k {
			return e.Value
		}
	}
	return "<unset>"
}

func TestCreatesChildrenFromSpec(t *testing.T) {
	r, c := setup(t, agent("alice", func(a *v1.Agent) { a.Spec.MaxConcurrent = 3 }))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	if *s.Spec.Replicas != 1 || envOf(s, "REPO") != "owner/repo" || envOf(s, "MAX_CONCURRENT") != "3" ||
		s.Spec.Template.Spec.Containers[0].Image != "worker:dev" {
		t.Fatalf("statefulset not rendered from spec: replicas=%d env=%v", *s.Spec.Replicas, s.Spec.Template.Spec.Containers[0].Env)
	}
	if s.Annotations["agentdev/repos"] != "owner/repo" || s.Labels["app"] != "coding-agent" {
		t.Errorf("router annotations/labels missing: %v %v", s.Annotations, s.Labels)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "alice" {
		t.Errorf("owner reference missing: %v", s.OwnerReferences)
	}
	if *s.Spec.Template.Spec.RuntimeClassName != "gvisor" || len(s.Spec.VolumeClaimTemplates) != 2 {
		t.Errorf("sandbox/volumes not rendered")
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice"}, &svc); err != nil {
		t.Errorf("service: %v", err)
	}
	var a v1.Agent
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "alice"}, &a)
	if a.Status.Phase != "starting" {
		t.Errorf("phase %q, want starting (no ready pod yet)", a.Status.Phase)
	}
}

func TestPausedScalesToZero(t *testing.T) {
	r, c := setup(t, agent("bob", func(a *v1.Agent) { a.Spec.Paused = true }))
	reconcile(t, r, "bob")
	if s := getSTS(t, c, "bob"); *s.Spec.Replicas != 0 {
		t.Fatalf("paused agent has replicas %d", *s.Spec.Replicas)
	}
	var a v1.Agent
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "bob"}, &a)
	if a.Status.Phase != "paused" {
		t.Errorf("phase %q", a.Status.Phase)
	}
}

func TestBlockedIsHonoured(t *testing.T) {
	a := agent("carol", nil)
	r, c := setup(t, a)
	reconcile(t, r, "carol")
	s := getSTS(t, c, "carol")
	s.Annotations["agentdev/blocked"] = "true" // scripts/agent-block.sh
	_ = c.Update(context.Background(), s)
	reconcile(t, r, "carol")
	if s := getSTS(t, c, "carol"); *s.Spec.Replicas != 0 {
		t.Fatalf("blocked agent restarted: replicas %d", *s.Spec.Replicas)
	}
}

func TestScaleToZeroKeepsRouterReplicas(t *testing.T) {
	r, c := setup(t, agent("dave", func(a *v1.Agent) { a.Spec.ScaleToZero = true }))
	reconcile(t, r, "dave")
	s := getSTS(t, c, "dave")
	s.Spec.Replicas = ptr.To[int32](0) // router stopped it when idle
	_ = c.Update(context.Background(), s)
	reconcile(t, r, "dave")
	if s := getSTS(t, c, "dave"); *s.Spec.Replicas != 0 {
		t.Fatalf("operator fought the router: replicas %d", *s.Spec.Replicas)
	}
}

func TestResumeStartsScaleToZeroAgent(t *testing.T) {
	r, c := setup(t, agent("erin", func(a *v1.Agent) { a.Spec.ScaleToZero = true; a.Spec.Paused = true }))
	reconcile(t, r, "erin")
	var a v1.Agent
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "erin"}, &a)
	a.Spec.Paused = false
	_ = c.Update(context.Background(), &a)
	reconcile(t, r, "erin")
	if s := getSTS(t, c, "erin"); *s.Spec.Replicas != 1 {
		t.Fatalf("resumed scale-to-zero agent not started: replicas %d", *s.Spec.Replicas)
	}
}

func TestMemoryFollowsMaxConcurrent(t *testing.T) {
	r, c := setup(t, agent("fay", func(a *v1.Agent) { a.Spec.MaxConcurrent = 4 }))
	reconcile(t, r, "fay")
	m := getSTS(t, c, "fay").Spec.Template.Spec.Containers[0].Resources.Limits.Memory()
	if m.Value() != 5<<30 {
		t.Fatalf("memory limit %s, want 5Gi for 4 slots", m)
	}
}

func TestDeletedAgentIsIgnored(t *testing.T) {
	r, _ := setup(t)
	reconcile(t, r, "gone") // NotFound must not error
}

func TestForeignSecretsRefused(t *testing.T) {
	for _, mut := range []func(*v1.Agent){
		func(a *v1.Agent) { a.Spec.ForgeSecret = "agent-bob" },
		func(a *v1.Agent) { a.Spec.ClaudeSecret = "claude-bob" },
	} {
		r, c := setup(t, agent("mallory", mut))
		reconcile(t, r, "mallory")
		var s appsv1.StatefulSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-mallory"}, &s); err == nil {
			t.Fatal("rendered a pod with another agent's Secret")
		}
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "mallory"}, &a)
		if a.Status.Phase != "invalid" {
			t.Errorf("phase %q, want invalid", a.Status.Phase)
		}
	}
	r, c := setup(t, agent("carl", func(a *v1.Agent) { a.Spec.ClaudeSecret = "claude-carl"; a.Spec.ForgeSecret = "agent-carl" }))
	reconcile(t, r, "carl")
	getSTS(t, c, "carl") // own Secrets are fine
}

func container(s *appsv1.StatefulSet, name string) *corev1.Container {
	for i := range s.Spec.Template.Spec.Containers {
		if c := &s.Spec.Template.Spec.Containers[i]; c.Name == name {
			return c
		}
	}
	return nil
}

func hasEnv(c *corev1.Container, k string) bool {
	for _, e := range c.Env {
		if e.Name == k {
			return true
		}
	}
	return false
}

// #64: forge credentials live only in the forge sidecar; the agent container has none, and cannot see the sidecar's
// processes.
func TestForgeSidecarHoldsTheTokens(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	p := s.Spec.Template.Spec
	ag, fg := container(s, "agent"), container(s, "forge")
	if ag == nil || fg == nil || len(p.Containers) != 2 {
		t.Fatalf("want containers agent and forge, got %d", len(p.Containers))
	}
	for _, k := range []string{"FORGE_TOKEN", "WAKE_TOKEN", "REGISTER_TOKEN", "SELF_URL"} {
		if hasEnv(ag, k) {
			t.Errorf("agent container has %s", k)
		}
	}
	for _, e := range ag.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("agent container reads a Secret key: %s", e.Name)
		}
	}
	for _, f := range ag.EnvFrom { // only the Claude credential and the MCP token, never the forge Secret
		if f.SecretRef != nil && f.SecretRef.Name == "agent-alice" {
			t.Error("agent container mounts the forge Secret")
		}
	}
	for _, k := range []string{"FORGE_TOKEN", "WAKE_TOKEN", "REGISTER_TOKEN", "SELF_URL", "ROUTER_URL"} {
		if !hasEnv(fg, k) {
			t.Errorf("forge container lacks %s", k)
		}
	}
	if fg.Image != "forge:dev" || len(fg.Ports) != 1 || fg.Ports[0].Name != "webhook" || len(ag.Ports) != 0 {
		t.Errorf("forge image/ports: %s %v / agent %v", fg.Image, fg.Ports, ag.Ports)
	}
	if p.ShareProcessNamespace == nil || *p.ShareProcessNamespace {
		t.Error("shareProcessNamespace must be false")
	}
	if fg.SecurityContext == nil || fg.SecurityContext.RunAsUser == nil || *fg.SecurityContext.RunAsUser == *p.SecurityContext.RunAsUser {
		t.Error("forge container must run as its own UID")
	}
	if p.SecurityContext.FSGroup == nil || *fg.SecurityContext.RunAsGroup != *p.SecurityContext.FSGroup {
		t.Error("forge container must share the fsGroup to write /state")
	}
	mounts := func(c *corev1.Container) map[string]string {
		m := map[string]string{}
		for _, v := range c.VolumeMounts {
			m[v.Name] = v.MountPath
		}
		return m
	}
	if mounts(ag)["state"] != "/state" || mounts(fg)["state"] != "/state" {
		t.Error("both containers must mount /state")
	}
	if _, ok := mounts(ag)["forge"]; ok || mounts(fg)["forge"] != "/forge" {
		t.Error("the sidecar's mirror volume is the sidecar's alone")
	}
}

// #65: with an egress proxy configured, both containers send their traffic through it; in-cluster names go direct.
func TestEgressProxyEnv(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	if hasEnv(container(s, "agent"), "HTTPS_PROXY") {
		t.Fatal("no proxy configured, but HTTPS_PROXY set")
	}
	r.D.EgressProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"
	reconcile(t, r, "alice")
	s = getSTS(t, c, "alice")
	for _, name := range []string{"agent", "forge"} {
		ct := container(s, name)
		got := map[string]string{}
		for _, e := range ct.Env {
			got[e.Name] = e.Value
		}
		want := r.D.EgressProxy
		if name == "forge" { // only the sidecar carries the forge-host credential (GitHub is the sidecar's)
			want = "http://forge:$(EGRESS_FORGE_KEY)@egress-proxy.coding-agents.svc.cluster.local:3128"
		}
		for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			if got[k] != want {
				t.Errorf("%s: %s=%q", name, k, got[k])
			}
		}
		if hasEnv(ct, "EGRESS_FORGE_KEY") != (name == "forge") {
			t.Errorf("%s: EGRESS_FORGE_KEY present=%v", name, hasEnv(ct, "EGRESS_FORGE_KEY"))
		}
		if !strings.Contains(got["NO_PROXY"], ".svc.cluster.local") || got["no_proxy"] != got["NO_PROXY"] {
			t.Errorf("%s: NO_PROXY=%q", name, got["NO_PROXY"])
		}
	}
}

// #65: in-cluster URLs given to agent pods are full service names, so NO_PROXY (.svc.cluster.local) sends them direct;
// a short name would go to the proxy and get a 403.
func TestInClusterURLsBypassProxy(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	r.D.EgressProxy, r.D.ForgeURL = "http://egress-proxy.coding-agents.svc.cluster.local:3128", "http://gitea.forge.svc.cluster.local:3000"
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	for _, ct := range s.Spec.Template.Spec.Containers {
		for _, e := range ct.Env {
			if !strings.HasSuffix(e.Name, "_URL") || !strings.HasPrefix(e.Value, "http://") || strings.HasPrefix(e.Value, "http://127.0.0.1") {
				continue
			}
			host := strings.Split(strings.TrimPrefix(e.Value, "http://"), "/")[0]
			host = strings.Split(host, ":")[0]
			if !strings.HasSuffix(host, ".svc.cluster.local") {
				t.Errorf("%s/%s=%s is not a full service name", ct.Name, e.Name, e.Value)
			}
		}
	}
}

// #66: with the credential proxy, the agent container holds no Claude credential, only its proxy key.
func TestClaudeProxyNoCredentialInPod(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	r.D.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	for _, ct := range s.Spec.Template.Spec.Containers {
		for _, f := range ct.EnvFrom {
			if f.SecretRef != nil && strings.HasPrefix(f.SecretRef.Name, "claude-") {
				t.Errorf("%s mounts %s", ct.Name, f.SecretRef.Name)
			}
		}
		for _, e := range ct.Env {
			if e.Name == "CLAUDE_CODE_OAUTH_TOKEN" || e.Name == "ANTHROPIC_API_KEY" {
				t.Errorf("%s has %s", ct.Name, e.Name)
			}
		}
	}
	ag := container(s, "agent")
	var base, key bool
	for _, e := range ag.Env {
		base = base || (e.Name == "ANTHROPIC_BASE_URL" && e.Value == r.D.ClaudeProxyURL)
		key = key || (e.Name == "ANTHROPIC_AUTH_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "agent-alice-proxy")
	}
	if !base || !key {
		t.Errorf("proxy env missing: %v", ag.Env)
	}
	for _, e := range container(s, "forge").Env {
		if e.Name == "USAGE_REPORTS" && e.Value != "off" {
			t.Errorf("forge USAGE_REPORTS=%s with the proxy on", e.Value)
		}
	}
	if hasEnv(container(s, "forge"), "ANTHROPIC_AUTH_TOKEN") {
		t.Error("forge sidecar has the proxy key")
	}
}

// #65 + #66: with both proxies, the agent container reaches claude-proxy directly (full service name, in NO_PROXY)
// and still sends everything else through the egress proxy.
func TestEgressAndClaudeProxy(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	r.D.EgressProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"
	r.D.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
	reconcile(t, r, "alice")
	got := map[string]string{}
	for _, e := range container(getSTS(t, c, "alice"), "agent").Env {
		got[e.Name] = e.Value
	}
	if got["HTTPS_PROXY"] != r.D.EgressProxy || got["ANTHROPIC_BASE_URL"] != r.D.ClaudeProxyURL {
		t.Fatalf("HTTPS_PROXY=%q ANTHROPIC_BASE_URL=%q", got["HTTPS_PROXY"], got["ANTHROPIC_BASE_URL"])
	}
	host := strings.Split(strings.TrimPrefix(got["ANTHROPIC_BASE_URL"], "http://"), ":")[0]
	bypass := false
	for _, s := range strings.Split(got["NO_PROXY"], ",") {
		bypass = bypass || (strings.HasPrefix(s, ".") && strings.HasSuffix(host, s))
	}
	if !bypass {
		t.Errorf("claude-proxy %s not in NO_PROXY=%q", host, got["NO_PROXY"])
	}
}
