package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #588: a security pause names the jobs and the events, marks the pod with the stop reason and writes an Event.
func TestPauseNamesJobsEventsAndMarksPod(t *testing.T) {
	p := pod("alice")
	col, c := collector(t, agent("alice", nil), p)
	rec := record.NewFakeRecorder(10)
	col.Recorder = rec
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		ts := time.Date(2026, 11, 25, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano)
		spec, _ := parseLine(line(ts, `{"source":"loop","rule":"git-config","severity":"low","job":"issue-5","action":"core.hooksPath `+string(rune('a'+i))+`"}`), p, "agent")
		if err := col.Record(ctx, p, spec); err != nil {
			t.Fatal(err)
		}
	}
	a := getAgent(t, c, "alice")
	cond := meta.FindStatusCondition(a.Status.Conditions, "SecurityPaused")
	if !a.Spec.Paused || cond == nil || cond.Reason != "TooManyEvents" {
		t.Fatalf("not paused: %+v", cond)
	}
	for _, want := range []string{"jobs with events in pod: issue-5", "loop/git-config x6", "core.hooksPath"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q lacks %q", cond.Message, want)
		}
	}
	var got corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: p.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[stopReasonAnnotation] != stopReasonSecurityPause {
		t.Fatalf("pod annotations: %v", got.Annotations)
	}
	select {
	case e := <-rec.Events:
		if !strings.HasPrefix(e, "Warning SecurityPaused TooManyEvents: ") || !strings.Contains(e, "issue-5") {
			t.Fatalf("event %q", e)
		}
	default:
		t.Fatal("no Kubernetes Event for the pause")
	}
}

func TestPauseDetail(t *testing.T) {
	trigger := v1.SecurityEventSpec{Source: "forge", Rule: "run-events-invalid", Severity: "medium", Job: "issue-5",
		Action: "run events refused: repo is not one of this agent's (repo other/x, 3 events, first event sha256 0a1b)"}
	items := []v1.SecurityEvent{
		{Spec: trigger}, // the event just recorded, by name below
		{Spec: v1.SecurityEventSpec{Source: "forge", Rule: "run-events-invalid", Severity: "medium", Job: "issue-5", Action: "again"}},
		{Spec: v1.SecurityEventSpec{Source: "loop", Rule: "hook", Severity: "low", Job: "pr-7", Action: "pre-commit"}},
		{Spec: v1.SecurityEventSpec{Source: "egress", Rule: "refused", Severity: "low", Job: "issue-5", Action: "example.com"}},
	}
	items[0].Name = "ev-0"
	got := pauseDetail(trigger, "ev-0", items)
	want := "jobs with events in pod: issue-5, pr-7; events: forge/run-events-invalid x2 (run events refused: repo is not one of this agent's (repo other/x, 3 events, first event sha256 0a1b))"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if d := pauseDetail(v1.SecurityEventSpec{Source: "forge", Rule: "push-branch"}, "", nil); d != "jobs with events in pod: none; events: forge/push-branch x1 ()" {
		t.Fatalf("no job: %q", d)
	}
}

// #588: spec.diagnostics off (unset or resources false) adds nothing to the pod; on, the worker gets the two variables.
func TestDiagnosticsEnv(t *testing.T) {
	a := agent("alice", nil)
	if e := diagnosticsEnv(a); e != nil {
		t.Fatalf("unset: %v", e)
	}
	a.Spec.Diagnostics = &v1.AgentDiagnostics{IntervalSeconds: 30}
	if e := diagnosticsEnv(a); e != nil {
		t.Fatalf("resources false: %v", e)
	}
	a.Spec.Diagnostics.Resources = true
	e := diagnosticsEnv(a)
	if len(e) != 2 || e[0].Name != "DIAG_RESOURCES" || e[0].Value != "true" || e[1].Name != "DIAG_INTERVAL_SECONDS" || e[1].Value != "30" {
		t.Fatalf("on: %v", e)
	}
	a.Spec.Diagnostics.IntervalSeconds = 0 // the worker's default
	if e := diagnosticsEnv(a); len(e) != 1 {
		t.Fatalf("no interval: %v", e)
	}
}
