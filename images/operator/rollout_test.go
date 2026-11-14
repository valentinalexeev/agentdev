package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// rolloutOf reads the agent's status.rollout.
func rolloutOf(t *testing.T, c client.Client, name string) v1.RolloutStatus {
	t.Helper()
	var a v1.Agent
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	if a.Status.Rollout == nil {
		return v1.RolloutStatus{}
	}
	return *a.Status.Rollout
}

// fakeNewPod plays the StatefulSet controller and the kubelet: pod 0 at the StatefulSet's current revision, ready
// since readySince, with the given restart count.
func fakeNewPod(t *testing.T, c client.Client, name string, readySince time.Time, restarts int32) {
	t.Helper()
	ctx := context.Background()
	s := getSTS(t, c, name)
	rev := "rev-" + templateVersionOf(s)
	s.Status.UpdateRevision = rev
	if err := c.Status().Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name + "-0", Namespace: ns}})
	p := agentPod(name, rev)
	p.UID = types.UID("pod-" + name + "-" + rev)
	p.Annotations = map[string]string{versionAnnotation: templateVersionOf(s)}
	p.CreationTimestamp = metav1.NewTime(readySince)
	p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(readySince)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", RestartCount: restarts}, {Name: "forge"}}
	if restarts > 0 {
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
	}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func alwaysOn(a *v1.Agent) { a.Spec.ScaleToZero = ptr.To(false) }

// three running agents on version A; the operator moves to version B
func rolloutSetup(t *testing.T) (*AgentReconciler, client.Client, *time.Time) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	r, c := setup(t, agent("ann", alwaysOn), agent("bob", alwaysOn), agent("cid", alwaysOn))
	r.Now, r.Drains = func() time.Time { return now }, &DrainSignals{}
	r.Rollout = RolloutConfig{Gate: 2 * time.Minute, GateTimeout: 15 * time.Minute}
	for _, n := range []string{"ann", "bob", "cid"} {
		reconcile(t, r, n)
		fakeNewPod(t, c, n, now.Add(-time.Hour), 0)
		reconcile(t, r, n)
		if p := rolloutOf(t, c, n).Phase; p != RolloutCurrent {
			t.Fatalf("%s on version A: phase %q", n, p)
		}
	}
	r.D.WorkerImage = "worker:B" // the upgrade
	return r, c, &now
}

func reconcileAll(t *testing.T, r *AgentReconciler) {
	for _, n := range []string{"ann", "bob", "cid"} {
		reconcile(t, r, n)
	}
}

func imageOf(t *testing.T, c client.Client, name string) string {
	return getSTS(t, c, name).Spec.Template.Spec.Containers[0].Image
}

func TestRolloutCanaryThenOneAtATime(t *testing.T) {
	r, c, now := rolloutSetup(t)
	r.Rollout.Canary = "bob"
	reconcileAll(t, r)
	if imageOf(t, c, "bob") != "worker:B" || imageOf(t, c, "ann") != "worker:dev" || imageOf(t, c, "cid") != "worker:dev" {
		t.Fatal("only the canary may move first")
	}
	if p := rolloutOf(t, c, "ann"); p.Phase != RolloutWaiting {
		t.Fatalf("ann: %+v, want Waiting for the canary", p)
	}
	// the canary's new pod is ready, but the gate time has not passed
	fakeNewPod(t, c, "bob", *now, 0)
	*now = now.Add(time.Minute)
	reconcileAll(t, r)
	if rolloutOf(t, c, "bob").Phase != RolloutUpdating || imageOf(t, c, "ann") != "worker:dev" {
		t.Fatal("gate not passed yet: nobody else moves")
	}
	*now = now.Add(2 * time.Minute)
	reconcileAll(t, r) // bob passes; ann moves (one at a time), cid waits for the slot
	if rolloutOf(t, c, "bob").Phase != RolloutCurrent {
		t.Fatalf("canary: %+v", rolloutOf(t, c, "bob"))
	}
	reconcileAll(t, r)
	first, second := "ann", "cid"
	if imageOf(t, c, "ann") != "worker:B" {
		first, second = "cid", "ann"
	}
	if imageOf(t, c, first) != "worker:B" || imageOf(t, c, second) != "worker:dev" {
		t.Fatalf("batch of 1: ann %s, cid %s", imageOf(t, c, "ann"), imageOf(t, c, "cid"))
	}
	if p := rolloutOf(t, c, second); p.Phase != RolloutWaiting {
		t.Fatalf("%s: %+v, want Waiting for a slot", second, p)
	}
	fakeNewPod(t, c, first, *now, 0)
	*now = now.Add(3 * time.Minute)
	reconcileAll(t, r)
	reconcileAll(t, r)
	if imageOf(t, c, second) != "worker:B" {
		t.Fatalf("%s did not move after %s passed", second, first)
	}
	fakeNewPod(t, c, second, *now, 0)
	*now = now.Add(3 * time.Minute)
	reconcileAll(t, r)
	for _, n := range []string{"ann", "bob", "cid"} {
		if p := rolloutOf(t, c, n); p.Phase != RolloutCurrent || p.Version != r.D.templateVersion() {
			t.Fatalf("%s: %+v", n, p)
		}
	}
}

func TestRolloutStopsOnFailedCanary(t *testing.T) {
	r, c, now := rolloutSetup(t) // canary: first by name, ann
	reconcileAll(t, r)
	fakeNewPod(t, c, "ann", *now, 2) // the broken image restarts
	reconcileAll(t, r)
	if p := rolloutOf(t, c, "ann"); p.Phase != RolloutFailed || p.Reason == "" {
		t.Fatalf("canary: %+v, want Failed with a reason", p)
	}
	*now = now.Add(time.Hour)
	reconcileAll(t, r)
	for _, n := range []string{"bob", "cid"} {
		if imageOf(t, c, n) != "worker:dev" {
			t.Fatalf("%s moved after the canary failed", n)
		}
		if p := rolloutOf(t, c, n); p.Phase != RolloutHalted {
			t.Fatalf("%s: %+v, want Halted", n, p)
		}
	}
	// rollback: the previous settings are a new version again; the canary moves back first and the rest follow
	r.D.WorkerImage = "worker:dev"
	reconcileAll(t, r)
	if imageOf(t, c, "ann") != "worker:dev" {
		t.Fatal("rollback did not move the canary back")
	}
	if p := rolloutOf(t, c, "bob"); p.Phase == RolloutHalted {
		t.Fatalf("bob still halted after the rollback: %+v", p)
	}
}

func TestRolloutGateTimeout(t *testing.T) {
	r, c, now := rolloutSetup(t)
	reconcileAll(t, r)
	fakeNewPod(t, c, "ann", *now, 0)
	var p corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-ann-0"}, &p)
	p.Status.Conditions[0].Status = corev1.ConditionFalse
	_ = c.Status().Update(context.Background(), &p)
	*now = now.Add(16 * time.Minute)
	reconcileAll(t, r)
	if st := rolloutOf(t, c, "ann"); st.Phase != RolloutFailed {
		t.Fatalf("never ready: %+v, want Failed", st)
	}
}

func TestStoppedAgentsMoveAfterCanary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r, c := setup(t, agent("ann", alwaysOn), agent("zed", nil)) // zed scales to zero
	r.Now = func() time.Time { return now }
	r.Rollout = RolloutConfig{Gate: time.Minute}
	reconcile(t, r, "ann")
	reconcile(t, r, "zed")
	fakeNewPod(t, c, "ann", now.Add(-time.Hour), 0)
	s := getSTS(t, c, "zed")
	s.Spec.Replicas = ptr.To[int32](0) // the router stopped it
	_ = c.Update(context.Background(), s)
	reconcile(t, r, "ann")
	reconcile(t, r, "zed")
	r.D.WorkerImage = "worker:B"
	reconcile(t, r, "ann")
	reconcile(t, r, "zed")
	if imageOf(t, c, "zed") != "worker:dev" {
		t.Fatal("a stopped agent must also wait for the canary")
	}
	fakeNewPod(t, c, "ann", now, 0)
	now = now.Add(2 * time.Minute)
	reconcile(t, r, "ann")
	reconcile(t, r, "zed")
	if imageOf(t, c, "zed") != "worker:B" {
		t.Fatal("stopped agent did not move after the canary")
	}
	if *getSTS(t, c, "zed").Spec.Replicas != 0 {
		t.Fatal("moving a stopped agent must not start it")
	}
}

func TestTemplateVersionFollowsSettings(t *testing.T) {
	d := Defaults{WorkerImage: "a"}
	v := d.templateVersion()
	if v != (Defaults{WorkerImage: "a"}).templateVersion() || v == (Defaults{WorkerImage: "b"}).templateVersion() {
		t.Fatal("version must be stable and change with the settings")
	}
	if (RolloutConfig{Canary: "x"}).canaryOf([]v1.Agent{*agent("b", nil), *agent("a", nil)}) != "a" {
		t.Fatal("unknown canary: the first agent by name")
	}
	_ = appsv1.OnDeleteStatefulSetStrategyType
}
