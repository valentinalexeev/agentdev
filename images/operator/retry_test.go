package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #593: an infrastructure kill is retried within spec.retries.infrastructure; a security pause, a deadline and a run's
// own error never are.
func TestRetryDecision(t *testing.T) {
	for _, tc := range []struct {
		reason           string
		attempt, retries int
		want             RetryDecision
	}{
		{"node-preempted", 2, 2, Retry},
		{"node-preempted", 3, 2, Retry},
		{"node-preempted", 4, 2, Exhausted}, // bounded: at most 1 + 2 attempts
		{"pod-deleted", 2, 2, Retry},
		{"drain", 2, 2, Retry},
		{"scale-down", 2, 2, Retry},
		{"upgrade", 2, 2, Retry},
		{"pod-deleted", 2, 0, Exhausted}, // 0: never retry
		{"security-pause", 2, 2, NoRetry},
		{"security-pause", 2, 10, NoRetry},
		{"deadline", 2, 2, NoRetry},
		{"oom", 2, 2, NoRetry},
		{"evicted", 2, 2, NoRetry},
		{"unknown", 2, 2, NoRetry},
		{"", 2, 2, NoRetry},
	} {
		if got := retryDecision(tc.reason, tc.attempt, tc.retries); got != tc.want {
			t.Errorf("retryDecision(%q, %d, %d) = %s, want %s", tc.reason, tc.attempt, tc.retries, got, tc.want)
		}
	}
}

func TestInfraRetriesDefaultAndEnv(t *testing.T) {
	a := agent("alice", nil)
	if infraRetries(a) != 2 || len(retriesEnv(a)) != 0 {
		t.Fatalf("default: %d %v", infraRetries(a), retriesEnv(a))
	}
	a.Spec.Retries = &v1.AgentRetries{Infrastructure: ptr.To[int32](2)}
	if len(retriesEnv(a)) != 0 {
		t.Fatal("the default value must not change the pod template")
	}
	a.Spec.Retries.Infrastructure = ptr.To[int32](0)
	if e := retriesEnv(a); len(e) != 1 || e[0].Name != "INFRA_RETRIES" || e[0].Value != "0" {
		t.Fatalf("env %v", e)
	}
	a.Spec.Retries.Infrastructure = ptr.To[int32](99)
	if infraRetries(a) != 10 {
		t.Fatal("clamped to 10")
	}
}

// an Agent without spec.retries renders the same pod template as before (no restart on upgrade)
func TestRetriesUnchangedTemplate(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	for _, e := range getSTS(t, c, "alice").Spec.Template.Spec.Containers[0].Env {
		if e.Name == "INFRA_RETRIES" {
			t.Fatal("INFRA_RETRIES without spec.retries")
		}
	}
	r, c = setup(t, agent("bob", func(a *v1.Agent) { a.Spec.Retries = &v1.AgentRetries{Infrastructure: ptr.To[int32](1)} }))
	reconcile(t, r, "bob")
	found := false
	for _, e := range getSTS(t, c, "bob").Spec.Template.Spec.Containers[0].Env {
		found = found || (e.Name == "INFRA_RETRIES" && e.Value == "1")
	}
	if !found {
		t.Fatal("INFRA_RETRIES=1 missing")
	}
}

// the loop and the operator retry the same reasons
func TestRetryReasonsMatchWorker(t *testing.T) {
	b, err := os.ReadFile("../worker/agent-loop.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^INFRA_RETRY_REASONS="([^"]*)"`).FindSubmatch(b)
	if m == nil || string(m[1]) != strings.Join(infraRetryReasons, " ") {
		t.Fatalf("worker INFRA_RETRY_REASONS %q, operator %q", m, infraRetryReasons)
	}
	if !regexp.MustCompile(`(?m)^INFRA_RETRIES="\$\{INFRA_RETRIES:-2\}"`).Match(b) {
		t.Fatal("worker default retries is not 2")
	}
}

func TestStopReasonOf(t *testing.T) {
	cond := func(reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reason}
	}
	spotNode := func(taints ...corev1.Taint) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{labelGKESpot: "true"}}, Spec: corev1.NodeSpec{Taints: taints}}
	}
	for _, tc := range []struct {
		name string
		mut  func(*corev1.Pod)
		node *corev1.Node
		want string
	}{
		{"nothing known", nil, nil, ""},
		{"the security pause mark wins", func(p *corev1.Pod) {
			p.Annotations = map[string]string{stopReasonAnnotation: "security-pause", drainAnnotation: "1"}
			p.Status.Conditions = []corev1.PodCondition{cond(corev1.PodReasonTerminationByKubelet)}
		}, nil, "security-pause"},
		{"graceful node shutdown", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{cond(corev1.PodReasonTerminationByKubelet)}
		}, nil, "node-preempted"},
		{"the Spot shutdown taint", nil, spotNode(corev1.Taint{Key: taintGKESpotPreempt, Effect: corev1.TaintEffectNoSchedule}), "node-preempted"},
		{"a Spot node no longer ready", nil, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelGKESpot: "true"}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}}}, "node-preempted"},
		{"a standard node no longer ready is not a preemption", nil, &corev1.Node{
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}, ""},
		{"eviction on a node the autoscaler removes", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{cond("EvictionByEvictionAPI")}
		}, spotNode(corev1.Taint{Key: taintAutoscalerDelete}), "scale-down"},
		{"eviction on a cordoned node", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{cond("EvictionByEvictionAPI")}
		}, &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}, "drain"},
		{"eviction, node unknown", func(p *corev1.Pod) { p.Status.Conditions = []corev1.PodCondition{cond("EvictionByEvictionAPI")} }, nil, "drain"},
		{"the operator's own upgrade", func(p *corev1.Pod) { p.Annotations = map[string]string{drainAnnotation: "1"} }, nil, "upgrade"},
		{"evicted by the kubelet", func(p *corev1.Pod) { p.Status.Reason = "Evicted" }, nil, "evicted"},
		{"a condition that is not true", func(p *corev1.Pod) {
			c := cond(corev1.PodReasonTerminationByKubelet)
			c.Status = corev1.ConditionFalse
			p.Status.Conditions = []corev1.PodCondition{c}
		}, nil, ""},
	} {
		p := pod("alice")
		if tc.mut != nil {
			tc.mut(p)
		}
		if got := stopReasonOf(p, tc.node); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// the collector records why a stopping pod stops in agentdev-usage (stop.<agent>), keeps the usage keys, writes one
// Event, and leaves a running pod alone
func TestNoteStopsRecordsReason(t *testing.T) {
	now := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	p := pod("alice")
	p.Spec.NodeName = "spot-1"
	p.DeletionTimestamp = &metav1.Time{Time: now}
	p.Finalizers = []string{"x"} // the fake client keeps an object with a deletion timestamp only with a finalizer
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "spot-1", Labels: map[string]string{labelGKESpot: "true"}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: taintGKESpotPreempt, Effect: corev1.TaintEffectNoSchedule}}}}
	running := pod("bob")
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageCM, Namespace: ns}, Data: map[string]string{"paused.carol": "123"}}
	col, c := collector(t, agent("alice", nil), agent("bob", nil), p, node, running, cm)
	col.Usage = &Usage{Client: c, Direct: c, NS: ns, Now: func() time.Time { return now }}
	rec := record.NewFakeRecorder(10)
	col.Recorder = rec
	ctx := context.Background()
	col.noteStops(ctx, []corev1.Pod{*p, *running})
	col.noteStops(ctx, []corev1.Pod{*p}) // again: one Event
	var got corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: usageCM}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data["stop.alice"] != "node-preempted 1795676400" || got.Data["paused.carol"] != "123" {
		t.Fatalf("data %v", got.Data)
	}
	if _, ok := got.Data["stop.bob"]; ok {
		t.Fatal("a running pod got a stop record")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events: %d", len(rec.Events))
	}
	if e := <-rec.Events; !strings.Contains(e, "PodStopped") || !strings.Contains(e, "node-preempted") || !strings.Contains(e, "at most 2 retries") {
		t.Fatalf("event %q", e)
	}
	// the usage evaluation rewrites its keys and keeps the stop record
	col.Usage.loaded = true
	col.Usage.subs, col.Usage.paused = map[string]*SubscriptionState{}, map[string]Pause{}
	if err := col.Usage.writeLocked(ctx, map[string]Pause{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: usageCM}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data["stop.alice"] == "" || got.Data["paused.carol"] != "" {
		t.Fatalf("after the usage write: %v", got.Data)
	}
}
