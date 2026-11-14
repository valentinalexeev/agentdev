package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func fwdPod(name, ns string, ready bool, lbl map[string]string) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbl},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}

func fwdWatch(t *testing.T, objs ...client.Object) (*ForwarderWatch, client.Client, *time.Time) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	w := &ForwarderWatch{Reader: c, Writer: c, NS: "tailscale", Selector: map[string]string{"app": "funnel"},
		EventsNS: "coding-agents", After: 10 * time.Minute, Now: func() time.Time { return now }}
	return w, c, &now
}

func TestForwarderNotReadyRecordsOneEventPerOutage(t *testing.T) {
	lbl := map[string]string{"app": "funnel"}
	p := fwdPod("funnel-0", "tailscale", false, lbl)
	other := fwdPod("other-0", "tailscale", true, map[string]string{"app": "other"}) // not selected: must not count
	w, c, now := fwdWatch(t, p, other)
	ctx := context.Background()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(9 * time.Minute)
	_ = w.Check(ctx)
	if n := len(events(t, c)); n != 0 {
		t.Fatalf("event before the threshold: %d", n)
	}
	*now = now.Add(2 * time.Minute)
	_ = w.Check(ctx)
	*now = now.Add(5 * time.Minute)
	_ = w.Check(ctx) // still down: no second event
	evs := events(t, c)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	s := evs[0].Spec
	if s.Source != "ingress" || s.Rule != "forwarder-not-ready" || s.Severity != "medium" || s.Agent != forwarderAgent ||
		s.Pod != "funnel-0" || s.Schema != v1.SchemaV1 || len(s.Hash) != 64 {
		t.Fatalf("spec: %+v", s)
	}
	for _, want := range []string{"namespace tailscale", "selector app=funnel", "1 pods, 0 Ready"} {
		if !contains(s.Action, want) {
			t.Fatalf("action %q misses %q", s.Action, want)
		}
	}
	// recovers, then a new outage gives a second event
	p.Status.Conditions[0].Status = corev1.ConditionTrue
	if err := c.Status().Update(ctx, p); err != nil {
		_ = c.Update(ctx, p)
	}
	*now = now.Add(time.Minute)
	_ = w.Check(ctx)
	if w.reported || !w.since.IsZero() {
		t.Fatal("state not reset after Ready")
	}
	p.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := c.Status().Update(ctx, p); err != nil {
		_ = c.Update(ctx, p)
	}
	_ = w.Check(ctx)
	*now = now.Add(11 * time.Minute)
	_ = w.Check(ctx)
	if n := len(events(t, c)); n != 2 {
		t.Fatalf("want 2 events after a second outage, got %d", n)
	}
}

func TestForwarderNoPodsCounts(t *testing.T) {
	w, c, now := fwdWatch(t)
	_ = w.Check(context.Background())
	*now = now.Add(11 * time.Minute)
	_ = w.Check(context.Background())
	evs := events(t, c)
	if len(evs) != 1 || !contains(evs[0].Spec.Action, "0 pods") {
		t.Fatalf("no pods must count as not Ready: %+v", evs)
	}
}

func TestForwarderReadyNoEvent(t *testing.T) {
	w, c, now := fwdWatch(t, fwdPod("funnel-0", "tailscale", false, map[string]string{"app": "funnel"}),
		fwdPod("funnel-1", "tailscale", true, map[string]string{"app": "funnel"}))
	_ = w.Check(context.Background())
	*now = now.Add(time.Hour)
	_ = w.Check(context.Background())
	if n := len(events(t, c)); n != 0 {
		t.Fatalf("one Ready pod is enough: %d events", n)
	}
}

func TestParseSelector(t *testing.T) {
	if m := parseSelector("app=funnel, tier = edge"); len(m) != 2 || m["tier"] != "edge" {
		t.Fatalf("%v", m)
	}
	for _, bad := range []string{"", " , ", "app", "=x"} {
		if m := parseSelector(bad); m != nil {
			t.Fatalf("%q: %v", bad, m)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
