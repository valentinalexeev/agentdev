package main

import (
	"context"
	"os"
	"regexp"
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
	if envOf(s, "MAX_TURNS") != "40" { // #336: default when the Agent has no maxTurns
		t.Errorf("MAX_TURNS default = %q, want 40", envOf(s, "MAX_TURNS"))
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "alice" {
		t.Errorf("owner reference missing: %v", s.OwnerReferences)
	}
	if *s.Spec.Template.Spec.RuntimeClassName != "gvisor" || len(s.Spec.VolumeClaimTemplates) != 1 {
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
	r, c := setup(t, agent("dave", func(a *v1.Agent) { a.Spec.ScaleToZero = ptr.To(true) }))
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
	r, c := setup(t, agent("erin", func(a *v1.Agent) { a.Spec.ScaleToZero = ptr.To(true); a.Spec.Paused = true }))
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

// #181: scale to zero is the default. An Agent without the field renders SCALE_TO_ZERO=true and the router
// annotation; an explicit false keeps the agent up and the operator owns its replicas.
func TestScaleToZeroIsTheDefault(t *testing.T) {
	r, c := setup(t, agent("gus", nil), agent("hana", func(a *v1.Agent) { a.Spec.ScaleToZero = ptr.To(false) }))
	reconcile(t, r, "gus")
	reconcile(t, r, "hana")
	g, h := getSTS(t, c, "gus"), getSTS(t, c, "hana")
	if envOf(g, "SCALE_TO_ZERO") != "true" || g.Annotations["agentdev/scale-to-zero"] != "true" || *g.Spec.Replicas != 1 {
		t.Fatalf("agent without scaleToZero: env %q, annotation %q, replicas %d", envOf(g, "SCALE_TO_ZERO"),
			g.Annotations["agentdev/scale-to-zero"], *g.Spec.Replicas)
	}
	if envOf(h, "SCALE_TO_ZERO") != "false" || h.Annotations["agentdev/scale-to-zero"] != "false" {
		t.Fatalf("scaleToZero false: env %q, annotation %q", envOf(h, "SCALE_TO_ZERO"), h.Annotations["agentdev/scale-to-zero"])
	}
	// the router stopped gus: the operator leaves it; hana at 0 by hand comes back (always up)
	for _, s := range []*appsv1.StatefulSet{g, h} {
		s.Spec.Replicas = ptr.To[int32](0)
		_ = c.Update(context.Background(), s)
	}
	reconcile(t, r, "gus")
	reconcile(t, r, "hana")
	if n := *getSTS(t, c, "gus").Spec.Replicas; n != 0 {
		t.Fatalf("default agent: operator fought the router: replicas %d", n)
	}
	if n := *getSTS(t, c, "hana").Spec.Replicas; n != 1 {
		t.Fatalf("scaleToZero false: not kept up: replicas %d", n)
	}
}

func TestCRDDefaultsScaleToZeroTrue(t *testing.T) {
	b, err := os.ReadFile("config/crd/agentdev.dev_agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)scaleToZero:\s*\n\s*default: true\n`).Match(b) {
		t.Fatal("CRD: scaleToZero has no default true (run controller-gen)")
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
	for _, e := range fg.Env { // registration and idle go to the router's internal port (#214)
		if e.Name == "ROUTER_URL" && e.Value != "http://webhook-router.coding-agents.svc.cluster.local:8081" {
			t.Errorf("ROUTER_URL = %q, want the internal port 8081", e.Value)
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
			if v.SubPath != "" { // ~/.claude is a directory of the state volume (#173)
				continue
			}
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

// #2: a GitHub agent gets the App credentials from its own Secret, in the forge container only, and github.com URLs.
func TestGitHubAgentCredentials(t *testing.T) {
	r, c := setup(t, agent("gina", func(a *v1.Agent) { a.Spec.Forge = "github"; a.Spec.Repo = "octo-org/repo" }))
	r.D.GitHubURL = "https://github.com"
	reconcile(t, r, "gina")
	s := getSTS(t, c, "gina")
	ag, fg := container(s, "agent"), container(s, "forge")
	for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_INSTALLATION_ID", "FORGE_TOKEN"} {
		if hasEnv(ag, k) {
			t.Errorf("agent container has %s", k)
		}
	}
	if hasEnv(fg, "FORGE_TOKEN") {
		t.Error("github forge container still asks for FORGE_TOKEN")
	}
	for _, e := range fg.Env {
		switch e.Name {
		case "GITHUB_APP_PRIVATE_KEY":
			t.Error("the private key must be a file, not an environment variable")
		case "GITHUB_APP_ID", "GITHUB_INSTALLATION_ID":
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != "agent-gina" {
				t.Errorf("%s not from agent-gina", e.Name)
			}
		case "FORGE_KIND":
			if e.Value != "github" {
				t.Errorf("FORGE_KIND %s", e.Value)
			}
		case "FORGE_URL":
			if e.Value != "https://github.com" {
				t.Errorf("FORGE_URL %s", e.Value)
			}
		}
	}
	if !hasEnv(fg, "GITHUB_APP_PRIVATE_KEY_FILE") || s.Annotations["agentdev/forge"] != "github" {
		t.Errorf("forge env or annotation missing: %v", s.Annotations)
	}
	mounted := func(c *corev1.Container) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == "github-app" {
				return m.ReadOnly
			}
		}
		return false
	}
	if !mounted(fg) || mounted(ag) {
		t.Error("the App key volume must be mounted read-only in the forge container only")
	}
	for _, v := range s.Spec.Template.Spec.Volumes {
		if v.Name == "github-app" && (v.Secret == nil || v.Secret.SecretName != "agent-gina" || len(v.Secret.Items) != 1) {
			t.Errorf("key volume: %+v", v)
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

// #2 + #66: a GitHub agent also goes through the Claude proxy and never holds the Claude credential.
func TestGitHubAgentUsesClaudeProxy(t *testing.T) {
	r, c := setup(t, agent("gina", func(a *v1.Agent) { a.Spec.Forge = "github"; a.Spec.Repo = "octo-org/repo" }))
	r.D.GitHubURL = "https://github.com"
	r.D.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
	reconcile(t, r, "gina")
	s := getSTS(t, c, "gina")
	for _, ct := range s.Spec.Template.Spec.Containers {
		for _, f := range ct.EnvFrom {
			if f.SecretRef != nil && strings.HasPrefix(f.SecretRef.Name, "claude-") {
				t.Errorf("%s mounts %s", ct.Name, f.SecretRef.Name)
			}
		}
		for _, e := range ct.Env {
			if e.Name == "CLAUDE_CODE_OAUTH_TOKEN" || e.Name == "ANTHROPIC_API_KEY" ||
				(e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.HasPrefix(e.ValueFrom.SecretKeyRef.Name, "claude-")) {
				t.Errorf("%s has %s", ct.Name, e.Name)
			}
		}
	}
	var base, key bool
	for _, e := range container(s, "agent").Env {
		base = base || (e.Name == "ANTHROPIC_BASE_URL" && e.Value == r.D.ClaudeProxyURL)
		key = key || (e.Name == "ANTHROPIC_AUTH_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "agent-gina-proxy")
	}
	if !base || !key {
		t.Error("github agent does not use the proxy")
	}
	if !hasEnv(container(s, "forge"), "GITHUB_APP_PRIVATE_KEY_FILE") || hasEnv(container(s, "agent"), "GITHUB_APP_ID") {
		t.Error("App credentials misplaced")
	}
}

// #2 + #65: a GitHub agent's forge sidecar reaches GitHub through the egress proxy with the forge credential, next to
// its App credentials; the agent container gets the plain proxy and neither the credential nor the App key.
func TestGitHubAgentEgressProxy(t *testing.T) {
	r, c := setup(t, agent("gina", func(a *v1.Agent) { a.Spec.Forge = "github"; a.Spec.Repo = "octo-org/repo" }))
	r.D.GitHubURL = "https://github.com"
	r.D.EgressProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"
	reconcile(t, r, "gina")
	s := getSTS(t, c, "gina")
	fg, ag := container(s, "forge"), container(s, "agent")
	got := map[string]string{}
	for _, e := range fg.Env {
		got[e.Name] = e.Value
	}
	if got["HTTPS_PROXY"] != "http://forge:$(EGRESS_FORGE_KEY)@egress-proxy.coding-agents.svc.cluster.local:3128" ||
		got["https_proxy"] != got["HTTPS_PROXY"] || !hasEnv(fg, "EGRESS_FORGE_KEY") {
		t.Errorf("forge proxy env: HTTPS_PROXY=%q", got["HTTPS_PROXY"])
	}
	if got["FORGE_KIND"] != "github" || got["FORGE_URL"] != "https://github.com" || !hasEnv(fg, "GITHUB_APP_ID") ||
		!hasEnv(fg, "GITHUB_APP_PRIVATE_KEY_FILE") {
		t.Errorf("forge github env missing: %v", got)
	}
	// github.com is not in NO_PROXY, so the sidecar's GitHub calls go through the proxy
	for _, d := range strings.Split(got["NO_PROXY"], ",") {
		if strings.HasSuffix("api.github.com", strings.TrimPrefix(d, ".")) && d != "" {
			t.Errorf("NO_PROXY %q bypasses the proxy for GitHub", d)
		}
	}
	// an EnvVar defined after its $(VAR) reference is not expanded by the kubelet
	key, proxy := -1, -1
	for i, e := range fg.Env {
		if e.Name == "EGRESS_FORGE_KEY" {
			key = i
		} else if e.Name == "HTTPS_PROXY" {
			proxy = i
		}
	}
	if key < 0 || key > proxy {
		t.Errorf("EGRESS_FORGE_KEY (%d) must precede HTTPS_PROXY (%d)", key, proxy)
	}
	for _, e := range ag.Env {
		if e.Name == "EGRESS_FORGE_KEY" || strings.HasPrefix(e.Name, "GITHUB_APP") || strings.Contains(e.Value, "EGRESS_FORGE_KEY") {
			t.Errorf("agent container has %s=%s", e.Name, e.Value)
		}
	}
}

func TestMaxTurnsFromSpec(t *testing.T) { // #336: maxTurns in the Agent reaches the worker as MAX_TURNS
	r, c := setup(t, agent("alice", func(a *v1.Agent) { a.Spec.MaxTurns = 120 }))
	reconcile(t, r, "alice")
	if got := envOf(getSTS(t, c, "alice"), "MAX_TURNS"); got != "120" {
		t.Fatalf("MAX_TURNS = %q, want 120", got)
	}
}

// #564: spec.model and spec.effort reach the worker as AGENT_MODEL and AGENT_EFFORT; unset: neither (CLI default).
func TestModelEffortFromSpec(t *testing.T) {
	r, c := setup(t, agent("alice", nil),
		agent("bob", func(a *v1.Agent) { a.Spec.Model, a.Spec.Effort = "claude-opus-5-5", "high" }),
		agent("carol", func(a *v1.Agent) { a.Spec.Model = "sonnet" }))
	for _, n := range []string{"alice", "bob", "carol"} {
		reconcile(t, r, n)
	}
	for _, tc := range []struct{ agent, model, effort string }{
		{"alice", "<unset>", "<unset>"}, {"bob", "claude-opus-5-5", "high"}, {"carol", "sonnet", "<unset>"},
	} {
		s := getSTS(t, c, tc.agent)
		if got := envOf(s, "AGENT_MODEL"); got != tc.model {
			t.Errorf("%s: AGENT_MODEL = %q, want %q", tc.agent, got, tc.model)
		}
		if got := envOf(s, "AGENT_EFFORT"); got != tc.effort {
			t.Errorf("%s: AGENT_EFFORT = %q, want %q", tc.agent, got, tc.effort)
		}
		for _, e := range s.Spec.Template.Spec.Containers[0].Env {
			if (e.Name == "AGENT_MODEL" && tc.model == "<unset>") || (e.Name == "AGENT_EFFORT" && tc.effort == "<unset>") {
				t.Errorf("%s: has %s without the field (pod template must stay unchanged)", tc.agent, e.Name)
			}
		}
	}
}

func TestModelEffortInvalidRefused(t *testing.T) {
	for name, mut := range map[string]func(*v1.Agent){
		"flag":    func(a *v1.Agent) { a.Spec.Model = "--dangerously-skip-permissions" },
		"space":   func(a *v1.Agent) { a.Spec.Model = "opus --effort max" },
		"newline": func(a *v1.Agent) { a.Spec.Model = "opus\nx" },
		"long":    func(a *v1.Agent) { a.Spec.Model = "claude-opus-" + strings.Repeat("a", 80) },
		"typo":    func(a *v1.Agent) { a.Spec.Model = "sonet" },  // #570: outside the price families
		"other":   func(a *v1.Agent) { a.Spec.Model = "gpt-5" },  // #570
		"upper":   func(a *v1.Agent) { a.Spec.Model = "Opus" },   // #570
		"suffix":  func(a *v1.Agent) { a.Spec.Model = "opus[2m]" }, // #570: only [1m]
		"effort":  func(a *v1.Agent) { a.Spec.Effort = "ultra" },
	} {
		r, c := setup(t, agent("mallory", mut))
		reconcile(t, r, "mallory")
		var s appsv1.StatefulSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-mallory"}, &s); err == nil {
			t.Errorf("%s: rendered a pod", name)
		}
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "mallory"}, &a)
		if a.Status.Phase != "invalid" {
			t.Errorf("%s: phase = %q, want invalid", name, a.Status.Phase)
		}
	}
}

// #500: spec.permission reaches the worker as CLAUDE_PERMISSION_MODE and CLAUDE_AUTO_MODE; dontAsk or unset: neither.
func TestPermissionFromSpec(t *testing.T) {
	r, c := setup(t, agent("alice", nil),
		agent("bob", func(a *v1.Agent) { a.Spec.Permission = &v1.AgentPermission{Mode: "dontAsk"} }),
		agent("carol", func(a *v1.Agent) { a.Spec.Permission = &v1.AgentPermission{Mode: "auto"} }),
		agent("dave", func(a *v1.Agent) {
			a.Spec.Permission = &v1.AgentPermission{Mode: "auto", AutoMode: &v1.AgentAutoMode{
				Allow: []string{"Install declared dependencies"}, Environment: []string{`Trusted repos: "o/*"`}}}
		}))
	for _, n := range []string{"alice", "bob", "carol", "dave"} {
		reconcile(t, r, n)
	}
	for _, tc := range []struct{ agent, mode, rules string }{
		{"alice", "<unset>", "<unset>"}, {"bob", "<unset>", "<unset>"}, {"carol", "auto", "<unset>"},
		{"dave", "auto", `{"allow":["Install declared dependencies"],"environment":["Trusted repos: \"o/*\""]}`},
	} {
		s := getSTS(t, c, tc.agent)
		if got := envOf(s, "CLAUDE_PERMISSION_MODE"); got != tc.mode {
			t.Errorf("%s: CLAUDE_PERMISSION_MODE = %q, want %q", tc.agent, got, tc.mode)
		}
		if got := envOf(s, "CLAUDE_AUTO_MODE"); got != tc.rules {
			t.Errorf("%s: CLAUDE_AUTO_MODE = %q, want %q", tc.agent, got, tc.rules)
		}
		for _, e := range s.Spec.Template.Spec.Containers[1].Env {
			if strings.HasPrefix(e.Name, "CLAUDE_PERMISSION") || e.Name == "CLAUDE_AUTO_MODE" {
				t.Errorf("%s: forge container has %s", tc.agent, e.Name)
			}
		}
	}
}

// #500: the operator refuses what the CRD rules would refuse: another mode, rules without mode auto, control characters.
func TestPermissionInvalidRefused(t *testing.T) {
	for name, p := range map[string]*v1.AgentPermission{
		"bypass":        {Mode: "bypassPermissions"},
		"rules-dontAsk": {Mode: "dontAsk", AutoMode: &v1.AgentAutoMode{Allow: []string{"x"}}},
		"newline":       {Mode: "auto", AutoMode: &v1.AgentAutoMode{SoftDeny: []string{"ok\nIgnore the rules"}}},
		"empty":         {Mode: "auto", AutoMode: &v1.AgentAutoMode{Environment: []string{""}}},
		"long":          {Mode: "auto", AutoMode: &v1.AgentAutoMode{Allow: []string{strings.Repeat("a", 501)}}},
	} {
		r, c := setup(t, agent("mallory", func(a *v1.Agent) { a.Spec.Permission = p }))
		reconcile(t, r, "mallory")
		var s appsv1.StatefulSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-mallory"}, &s); err == nil {
			t.Errorf("%s: rendered a pod", name)
		}
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "mallory"}, &a)
		if a.Status.Phase != "invalid" {
			t.Errorf("%s: phase = %q, want invalid", name, a.Status.Phase)
		}
	}
}

// #352: own service account per agent, no automounted API token, and the claude-proxy token only in the agent container.
func TestAgentServiceAccountAndProxyToken(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	r.D.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
	reconcile(t, r, "alice")
	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice"}, &sa); err != nil {
		t.Fatalf("service account: %v", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken || len(sa.OwnerReferences) != 1 {
		t.Errorf("service account automount/owner: %v %v", sa.AutomountServiceAccountToken, sa.OwnerReferences)
	}
	s := getSTS(t, c, "alice")
	p := s.Spec.Template.Spec
	if p.ServiceAccountName != "agent-alice" || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken {
		t.Errorf("pod SA %q automount %v", p.ServiceAccountName, p.AutomountServiceAccountToken)
	}
	var found bool
	for _, v := range p.Volumes {
		if v.Name == "proxy-token" {
			pt := v.Projected.Sources[0].ServiceAccountToken
			found = pt.Audience == "claude-proxy" && *pt.ExpirationSeconds == 600 && pt.Path == "token"
		}
	}
	if !found {
		t.Errorf("projected token volume missing or wrong: %v", p.Volumes)
	}
	mounted := func(ct corev1.Container) bool {
		for _, m := range ct.VolumeMounts {
			if m.Name == "proxy-token" {
				return m.MountPath == "/var/run/secrets/agentdev/proxy" && m.ReadOnly
			}
		}
		return false
	}
	if !mounted(*container(s, "agent")) {
		t.Error("agent container has no read-only proxy token")
	}
	for _, ct := range append(p.InitContainers, *container(s, "forge")) {
		for _, m := range ct.VolumeMounts {
			if m.Name == "proxy-token" {
				t.Errorf("%s mounts the proxy token", ct.Name)
			}
		}
	}
	// mixed versions: the HMAC key stays until the proxy runs in token mode
	if !hasEnv(container(s, "agent"), "ANTHROPIC_AUTH_TOKEN") {
		t.Error("ANTHROPIC_AUTH_TOKEN removed")
	}
	// without the proxy no token volume
	r2, c2 := setup(t, agent("bob", nil))
	reconcile(t, r2, "bob")
	for _, v := range getSTS(t, c2, "bob").Spec.Template.Spec.Volumes {
		if v.Name == "proxy-token" {
			t.Error("token volume without claude-proxy")
		}
	}
}

// #496: spec.tools.allow reaches the agent container only, as CLAUDE_TOOLS_EXTRA; unset or empty: no env (no restart).
func TestToolsFromSpec(t *testing.T) {
	r, c := setup(t, agent("alice", nil),
		agent("bob", func(a *v1.Agent) { a.Spec.Tools = &v1.AgentTools{} }),
		agent("carol", func(a *v1.Agent) {
			a.Spec.Tools = &v1.AgentTools{Allow: []string{"Bash(npm ci:*)", "Bash(npm install:*)"}}
		}))
	for _, n := range []string{"alice", "bob", "carol"} {
		reconcile(t, r, n)
	}
	for _, tc := range []struct{ agent, extra string }{
		{"alice", "<unset>"}, {"bob", "<unset>"}, {"carol", "Bash(npm ci:*),Bash(npm install:*)"},
	} {
		s := getSTS(t, c, tc.agent)
		if got := envOf(s, "CLAUDE_TOOLS_EXTRA"); got != tc.extra {
			t.Errorf("%s: CLAUDE_TOOLS_EXTRA = %q, want %q", tc.agent, got, tc.extra)
		}
		for _, ct := range s.Spec.Template.Spec.Containers[1:] {
			for _, e := range ct.Env {
				if strings.HasPrefix(e.Name, "CLAUDE_TOOLS") {
					t.Errorf("%s: container %s has %s", tc.agent, ct.Name, e.Name)
				}
			}
		}
	}
}

// #496: the operator refuses what the CRD rules would refuse: commas, control characters, empty, too long, too many.
func TestToolsInvalidRefused(t *testing.T) {
	many := make([]string, 51)
	for i := range many {
		many[i] = "Read"
	}
	for name, l := range map[string][]string{
		"comma":   {"Bash(npm ci:*),Bash(curl:*)"},
		"newline": {"Read\nBash(curl:*)"},
		"empty":   {""},
		"long":    {strings.Repeat("a", 201)},
		"many":    many,
	} {
		r, c := setup(t, agent("mallory", func(a *v1.Agent) { a.Spec.Tools = &v1.AgentTools{Allow: l} }))
		reconcile(t, r, "mallory")
		var s appsv1.StatefulSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-mallory"}, &s); err == nil {
			t.Errorf("%s: rendered a pod", name)
		}
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "mallory"}, &a)
		if a.Status.Phase != "invalid" {
			t.Errorf("%s: phase = %q, want invalid", name, a.Status.Phase)
		}
	}
}
