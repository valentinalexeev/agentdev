package main

import (
	"context"
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
	return &AgentReconciler{Client: c, D: Defaults{WorkerImage: "worker:dev", PullPolicy: "Never", ForgeURL: "http://forge",
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
		envOf(s, "FORGE_KIND") != "gitea" || s.Spec.Template.Spec.Containers[0].Image != "worker:dev" {
		t.Fatalf("statefulset not rendered from spec: replicas=%d env=%v", *s.Spec.Replicas, s.Spec.Template.Spec.Containers[0].Env)
	}
	if s.Annotations["agentdev/repos"] != "owner/repo" || s.Labels["app"] != "coding-agent" {
		t.Errorf("router annotations/labels missing: %v %v", s.Annotations, s.Labels)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "alice" {
		t.Errorf("owner reference missing: %v", s.OwnerReferences)
	}
	if *s.Spec.Template.Spec.RuntimeClassName != "gvisor" || len(s.Spec.VolumeClaimTemplates) != 2 {
		t.Errorf("sandbox/volumes not as in agent.yaml.tmpl")
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
