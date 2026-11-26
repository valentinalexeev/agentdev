package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #577: a security pause for many low events drains the pod (running jobs finish); a high-severity event stops it at
// once; repeated refusals of run-event batches count once; the Resumed condition names who lifted the pause.

func recordN(t *testing.T, col *Collector, p *corev1.Pod, n int, raw string) {
	t.Helper()
	for i := 0; i < n; i++ {
		ts := time.Date(2026, 11, 25, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano)
		spec, ok := parseLine(line(ts, raw), p, "forge")
		if !ok {
			t.Fatalf("line not parsed: %s", raw)
		}
		if err := col.Record(context.Background(), p, spec); err != nil {
			t.Fatal(err)
		}
	}
}

func podOf(t *testing.T, c client.Client, name string) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-" + name + "-0"}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestSecurityPauseDrainsOnTooManyLowEvents(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := agentPod("alice", "rev1")
	r, c := setup(t, agent("alice", nil), p)
	r.Drains, r.Now, r.D.DrainMaxMinutes = &DrainSignals{}, func() time.Time { return now }, "60"
	col := &Collector{Client: c, Direct: c, NS: ns, Now: func() time.Time { return now }, C: SecurityConfig{PauseAfter: 5}}
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); s.Spec.Replicas == nil || *s.Spec.Replicas != 1 {
		t.Fatal("agent not running before the pause")
	}

	recordN(t, col, p, 6, `{"rule":"hook","severity":"low","job":"issue-5","action":"x"}`)
	a := getAgent(t, c, "alice")
	if !a.Spec.Paused || a.Annotations[pauseDrainAnnotation] == "" {
		t.Fatalf("six low events: paused %v, drain mark %q", a.Spec.Paused, a.Annotations[pauseDrainAnnotation])
	}

	// 1. paused, jobs still running: the pod stays and is marked for drain (no new jobs)
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); s.Spec.Replicas == nil || *s.Spec.Replicas != 1 {
		t.Fatal("six low events stopped the running pod at once")
	}
	if pa := podOf(t, c, "alice").Annotations; pa[drainAnnotation] == "" || pa[drainReasonAnnotation] != drainReasonSecurity {
		t.Fatalf("pod not marked for a security-pause drain (#581): %v", pa)
	}
	// 2. its sidecar reports drained: stopped
	r.Drains.Mark(p.UID, now)
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); s.Spec.Replicas == nil || *s.Spec.Replicas != 0 {
		t.Fatal("drained pod not stopped")
	}
}

func TestSecurityPauseDrainTimeout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := agentPod("alice", "rev1")
	r, c := setup(t, agent("alice", nil), p)
	r.Now, r.D.DrainMaxMinutes = func() time.Time { return now }, "60"
	reconcile(t, r, "alice")
	a := getAgent(t, c, "alice")
	a.Spec.Paused, a.Annotations = true, map[string]string{pauseDrainAnnotation: "1800000000"}
	if err := c.Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); s.Spec.Replicas == nil || *s.Spec.Replicas != 1 {
		t.Fatal("stopped before the drain time")
	}
	now = now.Add(61 * time.Minute)
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); s.Spec.Replicas == nil || *s.Spec.Replicas != 0 {
		t.Fatal("not stopped after the drain time")
	}
}

func TestSecurityPauseHighStopsAtOnce(t *testing.T) {
	p := agentPod("alice", "rev1")
	r, c := setup(t, agent("alice", nil), p)
	col := &Collector{Client: c, Direct: c, NS: ns, Now: time.Now, C: SecurityConfig{PauseAfter: 5}}
	reconcile(t, r, "alice")
	// a drained pause is under way; a high-severity event ends it
	recordN(t, col, p, 6, `{"rule":"hook","severity":"low","job":"issue-5","action":"x"}`)
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); *s.Spec.Replicas != 1 {
		t.Fatal("low events: not draining")
	}
	spec, _ := parseLine(line(ts, `{"rule":"push-branch","severity":"high","job":"issue-5","action":"push main"}`), p, "forge")
	if err := col.Record(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	a := getAgent(t, c, "alice")
	if _, ok := a.Annotations[pauseDrainAnnotation]; ok || !a.Spec.Paused {
		t.Fatalf("high event: drain mark kept %v, paused %v", a.Annotations, a.Spec.Paused)
	}
	if cond := meta.FindStatusCondition(a.Status.Conditions, "SecurityPaused"); cond == nil || cond.Reason != "HighSeverity" {
		t.Fatalf("condition %+v", cond)
	}
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); *s.Spec.Replicas != 0 {
		t.Fatal("high-severity pause did not stop the pod at once")
	}
}

func TestPersonPauseStopsAtOnceAndResumeUnmarks(t *testing.T) {
	p := agentPod("alice", "rev1")
	r, c := setup(t, agent("alice", func(a *v1.Agent) { a.Spec.Paused = true }), p)
	reconcile(t, r, "alice")
	if s := getSTS(t, c, "alice"); *s.Spec.Replicas != 0 {
		t.Fatal("a pause by a person must stop at once")
	}

	// resumed during a drained pause: both marks go, the pod takes jobs again
	r2, c2 := setup(t, agent("bob", func(a *v1.Agent) {
		a.Annotations = map[string]string{pauseDrainAnnotation: "1800000000"}
	}), func() *corev1.Pod {
		q := agentPod("bob", "rev1")
		q.Annotations = map[string]string{drainAnnotation: "1800000000", drainReasonAnnotation: drainReasonSecurity}
		return q
	}())
	reconcile(t, r2, "bob")
	if _, ok := getAgent(t, c2, "bob").Annotations[pauseDrainAnnotation]; ok {
		t.Fatal("drain mark kept on the resumed agent")
	}
	if pa := podOf(t, c2, "bob").Annotations; pa[drainAnnotation] != "" || pa[drainReasonAnnotation] != "" {
		t.Fatalf("resumed pod still drained: %v", pa)
	}
}

func TestRefusedRunEventsCountOnce(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	p := pod("alice")
	recordN(t, col, p, 12, `{"rule":"run-events-invalid","severity":"low","job":"issue-5",`+
		`"action":"run events refused: repo is not one of this agent's (repo o/r, 1 events, first event sha256 0a1b2c3d4e5f)"}`)
	if getAgent(t, c, "alice").Spec.Paused {
		t.Fatal("one repeated refusal paused the agent")
	}
	if n := len(events(t, c)); n != 12 {
		t.Fatalf("every refusal is still recorded: %d", n)
	}
	// other events of the job still count, the refusal once
	recordN(t, col, p, 5, `{"rule":"hook","severity":"low","job":"issue-5","action":"y"}`)
	if !getAgent(t, c, "alice").Spec.Paused {
		t.Fatal("6 distinct events did not pause")
	}
}

func TestResumedNamesWhoLiftedThePause(t *testing.T) {
	t0, t1 := metav1.NewTime(time.Unix(1_800_000_000, 0)), metav1.NewTime(time.Unix(1_800_000_100, 0))
	a := agent("alice", nil)
	a.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: OperatorFieldManager, Time: &t0, FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:paused":{}}}`)}},
		{Manager: "kubectl-patch", Time: &t1, FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:paused":{}}}`)}},
		{Manager: "flux", Time: &t1, FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:repo":{}}}`)}},
	}
	if by := pausedBy(a); by != "kubectl-patch" {
		t.Fatalf("by %q", by)
	}
	if by := pausedBy(agent("bob", nil)); by != "unknown" {
		t.Fatalf("no managed fields: %q", by)
	}

	r, c := setup(t, agent("carol", func(a *v1.Agent) {
		a.Status.Conditions = []metav1.Condition{{Type: "SecurityPaused", Status: metav1.ConditionTrue, Reason: "TooManyEvents",
			Message: "6 security events", LastTransitionTime: t0}}
	}))
	reconcile(t, r, "carol")
	cond := meta.FindStatusCondition(getAgent(t, c, "carol").Status.Conditions, "SecurityPaused")
	if cond == nil || cond.Reason != "Resumed" || !strings.HasPrefix(cond.Message, "resumed by ") || !strings.Contains(cond.Message, "6 security events") {
		t.Fatalf("condition %+v", cond)
	}
}
