package main

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// agentPod is pod 0 of agent-<name> at template revision rev, running and ready.
func agentPod(name, rev string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name + "-0", Namespace: ns, UID: types.UID("pod-" + rev),
		Labels: map[string]string{"app": "coding-agent", "agent": name, appsv1.ControllerRevisionHashLabelKey: rev}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

// withRevision sets the StatefulSet's update revision, as the StatefulSet controller would after a template change.
func withRevision(t *testing.T, c client.Client, name, rev string) {
	t.Helper()
	s := getSTS(t, c, name)
	s.Status.UpdateRevision = rev
	if err := c.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func podGone(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var p corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-" + name + "-0"}, &p)
	return apierrors.IsNotFound(err)
}

func updated(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()
	var a v1.Agent
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(a.Status.Conditions, UpdatedCondition)
}

func TestAgentPodsUseOnDeleteAndGrace(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	if s.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatalf("update strategy %q, want OnDelete", s.Spec.UpdateStrategy.Type)
	}
	if g := s.Spec.Template.Spec.TerminationGracePeriodSeconds; g == nil || *g != 300 {
		t.Fatalf("grace %v, want 300", g)
	}
	r.D.GraceSeconds = "5" // out of range: the default
	if r.D.graceSeconds() != 300 {
		t.Fatal("grace below 30 s must not be taken")
	}
}

func TestDrainBeforeReplace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r, c := setup(t, agent("alice", nil), agentPod("alice", "old"))
	r.Drains, r.Now, r.D.DrainMaxMinutes = &DrainSignals{}, func() time.Time { return now }, "60"
	reconcile(t, r, "alice")
	withRevision(t, c, "alice", "new")

	// 1. outdated and busy: marked for drain, not deleted
	reconcile(t, r, "alice")
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice-0"}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Annotations[drainAnnotation] == "" {
		t.Fatal("outdated pod was not marked for drain")
	}
	if c := updated(t, c, "alice"); c == nil || c.Reason != "Draining" {
		t.Fatalf("condition %+v, want Draining", c)
	}

	// 2. a drained line from another pod does not count
	r.Drains.Mark("someone-else", now)
	reconcile(t, r, "alice")
	if podGone(t, c, "alice") {
		t.Fatal("pod replaced without its own drain report")
	}

	// 3. its own sidecar reports drained: replaced
	r.Drains.Mark(p.UID, now)
	reconcile(t, r, "alice")
	if !podGone(t, c, "alice") {
		t.Fatal("drained pod was not replaced")
	}
	if c := updated(t, c, "alice"); c == nil || c.Reason != "Drained" {
		t.Fatalf("condition %+v, want Drained", c)
	}
}

func TestDrainTimeoutForcesReplace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r, c := setup(t, agent("alice", nil), agentPod("alice", "old"))
	r.Now, r.D.DrainMaxMinutes = func() time.Time { return now }, "60"
	reconcile(t, r, "alice")
	withRevision(t, c, "alice", "new")
	reconcile(t, r, "alice")
	now = now.Add(59 * time.Minute)
	reconcile(t, r, "alice")
	if podGone(t, c, "alice") {
		t.Fatal("replaced before the maximum wait")
	}
	now = now.Add(2 * time.Minute)
	reconcile(t, r, "alice")
	if !podGone(t, c, "alice") {
		t.Fatal("not replaced after the maximum wait")
	}
	if c := updated(t, c, "alice"); c == nil || c.Reason != "Forced" {
		t.Fatalf("condition %+v, want Forced", c)
	}
}

func TestCurrentOrStoppedPodIsLeftAlone(t *testing.T) {
	r, c := setup(t, agent("alice", nil), agentPod("alice", "new"))
	reconcile(t, r, "alice")
	withRevision(t, c, "alice", "new")
	reconcile(t, r, "alice")
	if podGone(t, c, "alice") {
		t.Fatal("current pod replaced")
	}
	if c := updated(t, c, "alice"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v, want Updated=True", c)
	}
	r2, c2 := setup(t, agent("bob", nil)) // no pod (scaled to zero)
	reconcile(t, r2, "bob")
	withRevision(t, c2, "bob", "new")
	reconcile(t, r2, "bob")
	if c := updated(t, c2, "bob"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("stopped agent: %+v, want Updated=True", c)
	}
}

func TestNotReadyOutdatedPodReplacedAtOnce(t *testing.T) {
	p := agentPod("alice", "old")
	p.Status.Phase = corev1.PodPending
	r, c := setup(t, agent("alice", nil), p)
	reconcile(t, r, "alice")
	withRevision(t, c, "alice", "new")
	reconcile(t, r, "alice")
	if !podGone(t, c, "alice") {
		t.Fatal("a pod that does not run has no jobs to wait for")
	}
}

func TestDrainedLineOnlyFromForge(t *testing.T) {
	if !isDrainedLine("2026-11-07T10:00:00.1Z AGENTDEV_DRAINED") || !isDrainedLine("2026-11-07T10:00:00Z AGENTDEV_DRAINED {}\n") {
		t.Fatal("drained line not recognised")
	}
	for _, l := range []string{"AGENTDEV_DRAINED", "2026-11-07T10:00:00Z echo AGENTDEV_DRAINED", "2026-11-07T10:00:00Z AGENTDEV_DRAINEDX"} {
		if isDrainedLine(l) {
			t.Fatalf("%q taken as drained", l)
		}
	}
	d := &DrainSignals{}
	col := &Collector{Drains: d, Now: time.Now}
	p := agentPod("alice", "old")
	line := "2026-11-07T10:00:00Z AGENTDEV_DRAINED\n"
	_ = col.consume(context.Background(), strings.NewReader(line), p, "agent")
	if d.Drained(p.UID) {
		t.Fatal("drained line from the agent container (Claude can print it) was taken")
	}
	_ = col.consume(context.Background(), strings.NewReader(line), p, forgeContainer)
	if !d.Drained(p.UID) {
		t.Fatal("drained line from the forge container was not taken")
	}
}
