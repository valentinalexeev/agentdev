package main

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #386: Ready is True for every phase in which the router can wake the agent, False for invalid and blocked.
func TestAgentReadyEachPhase(t *testing.T) {
	for _, tc := range []struct {
		phase      string
		reconciled bool
		want       metav1.ConditionStatus
		reason     string
	}{
		{"running", true, metav1.ConditionTrue, "Reconciled"},
		{"starting", true, metav1.ConditionTrue, "Reconciled"},
		{"stopped", true, metav1.ConditionTrue, "Reconciled"},
		{"paused", true, metav1.ConditionTrue, "Reconciled"},
		{"blocked", true, metav1.ConditionFalse, "Blocked"},
		{"invalid", false, metav1.ConditionFalse, "SecretNotBound"},
		{"running", false, metav1.ConditionFalse, "SecretNotBound"},
	} {
		c := agentReady(tc.phase, tc.reconciled, "SecretNotBound", "msg", 7)
		if c.Type != "Ready" || c.Status != tc.want || c.Reason != tc.reason || c.ObservedGeneration != 7 {
			t.Errorf("%s reconciled=%v: got %s/%s gen %d, want %s/%s", tc.phase, tc.reconciled, c.Status, c.Reason,
				c.ObservedGeneration, tc.want, tc.reason)
		}
	}
}

func readyOf(t *testing.T, conds []metav1.Condition) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(conds, "Ready")
	if c == nil {
		t.Fatalf("no Ready condition in %v", conds)
	}
	return c
}

func readyAgent(t *testing.T, r *AgentReconciler, name string) v1.Agent {
	t.Helper()
	var a v1.Agent
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// A stopped (scaled to zero) agent is Ready, so a Flux Kustomization with wait: true stays healthy while agents sleep.
func TestStoppedAgentIsReady(t *testing.T) {
	r, c := setup(t, agent("sleepy", func(a *v1.Agent) { a.Spec.ScaleToZero = ptr.To(true); a.Generation = 3 }))
	reconcile(t, r, "sleepy")
	s := getSTS(t, c, "sleepy")
	s.Spec.Replicas = ptr.To[int32](0) // router stopped it when idle
	_ = c.Update(context.Background(), s)
	reconcile(t, r, "sleepy")
	a := readyAgent(t, r, "sleepy")
	if a.Status.Phase != "stopped" {
		t.Fatalf("phase %q, want stopped", a.Status.Phase)
	}
	if rc := readyOf(t, a.Status.Conditions); rc.Status != metav1.ConditionTrue || rc.ObservedGeneration != a.Generation {
		t.Errorf("stopped agent Ready=%s gen %d, want True gen %d", rc.Status, rc.ObservedGeneration, a.Generation)
	}
	if a.Status.ObservedGeneration != a.Generation {
		t.Errorf("observedGeneration %d, want %d", a.Status.ObservedGeneration, a.Generation)
	}
	if a.Status.Image != "worker:dev" {
		t.Errorf("status.image %q, want worker:dev", a.Status.Image)
	}
}

func TestPausedAndStartingAgentAreReady(t *testing.T) {
	r, _ := setup(t, agent("p", func(a *v1.Agent) { a.Spec.Paused = true }), agent("s", nil))
	reconcile(t, r, "p")
	reconcile(t, r, "s")
	for _, n := range []string{"p", "s"} {
		a := readyAgent(t, r, n)
		if rc := readyOf(t, a.Status.Conditions); rc.Status != metav1.ConditionTrue {
			t.Errorf("%s (%s): Ready=%s, want True", n, a.Status.Phase, rc.Status)
		}
	}
}

func TestBlockedAgentIsNotReady(t *testing.T) {
	r, c := setup(t, agent("blk", nil))
	reconcile(t, r, "blk")
	s := getSTS(t, c, "blk")
	s.Annotations["agentdev/blocked"] = "true"
	_ = c.Update(context.Background(), s)
	reconcile(t, r, "blk")
	a := readyAgent(t, r, "blk")
	if rc := readyOf(t, a.Status.Conditions); rc.Status != metav1.ConditionFalse || rc.Reason != "Blocked" {
		t.Errorf("blocked agent Ready=%s/%s, want False/Blocked", rc.Status, rc.Reason)
	}
}

func TestInvalidAgentIsNotReady(t *testing.T) {
	r, _ := setup(t, agent("bad", func(a *v1.Agent) { a.Spec.ForgeSecret = "agent-other" }))
	reconcile(t, r, "bad")
	a := readyAgent(t, r, "bad")
	if rc := readyOf(t, a.Status.Conditions); rc.Status != metav1.ConditionFalse || rc.Reason != "SecretNotBound" {
		t.Errorf("invalid agent Ready=%s/%s, want False/SecretNotBound", rc.Status, rc.Reason)
	}
}

// A new generation moves Ready's observedGeneration, so kstatus does not read a stale Ready as current.
func TestReadyFollowsGeneration(t *testing.T) {
	r, c := setup(t, agent("gen", func(a *v1.Agent) { a.Generation = 1 }))
	reconcile(t, r, "gen")
	a := readyAgent(t, r, "gen")
	a.Generation = 2
	a.Spec.MaxConcurrent = 2
	if err := c.Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "gen")
	a = readyAgent(t, r, "gen")
	if rc := readyOf(t, a.Status.Conditions); rc.ObservedGeneration != a.Generation || a.Status.ObservedGeneration != a.Generation {
		t.Errorf("Ready gen %d, status gen %d, want %d", rc.ObservedGeneration, a.Status.ObservedGeneration, a.Generation)
	}
}

func TestProjectSecretReady(t *testing.T) {
	for phase, want := range map[string]metav1.ConditionStatus{"ready": metav1.ConditionTrue,
		"pending": metav1.ConditionFalse, "error": metav1.ConditionFalse} {
		if c := projectSecretReady(phase, "", 4); c.Status != want || c.ObservedGeneration != 4 || c.Reason == "" || c.Message == "" {
			t.Errorf("%s: %+v", phase, c)
		}
	}
}
