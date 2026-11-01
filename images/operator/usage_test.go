package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func TestEffectiveThresholds(t *testing.T) {
	cases := []struct {
		name string
		s    UsageSettings
		repo string
		want Thresholds
	}{
		{"defaults", UsageSettings{}, "o/r", Thresholds{80, 90}},
		{"global set", UsageSettings{Global: Thresholds{70, 85}}, "o/r", Thresholds{70, 85}},
		{"invalid global ignored", UsageSettings{Global: Thresholds{0, 150}}, "o/r", Thresholds{80, 90}},
		{"project lower", UsageSettings{Projects: map[string]Thresholds{"o/r": {50, 60}}}, "o/r", Thresholds{50, 60}},
		{"project higher is clamped", UsageSettings{Global: Thresholds{70, 85}, Projects: map[string]Thresholds{"o/r": {95, 99}}}, "o/r", Thresholds{70, 85}},
		{"project partial", UsageSettings{Projects: map[string]Thresholds{"o/r": {FiveHour: 40}}}, "o/r", Thresholds{40, 90}},
		{"other project unaffected", UsageSettings{Projects: map[string]Thresholds{"o/x": {10, 10}}}, "o/r", Thresholds{80, 90}},
	}
	for _, c := range cases {
		if got := c.s.Effective(c.repo); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestEffectiveAll(t *testing.T) {
	s := UsageSettings{Global: Thresholds{80, 90}, Projects: map[string]Thresholds{"o/a": {60, 0}, "o/b": {0, 70}}}
	if got := s.EffectiveAll("o/a", "o/b"); got != (Thresholds{60, 70}) {
		t.Fatalf("lowest over all repos: %+v", got)
	}
	if got := s.EffectiveAll(); got != (Thresholds{80, 90}) {
		t.Fatalf("no repo: global: %+v", got)
	}
}

func TestDecide(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset5, resetW := now.Add(2*time.Hour).Unix(), now.Add(72*time.Hour).Unix()
	th := Thresholds{80, 90}
	s := &SubscriptionState{FiveHour: Window{0.79, reset5}, Weekly: Window{0.5, resetW}}
	if _, ok := decide(s, th, now); ok {
		t.Fatal("below the threshold: no pause")
	}
	s.FiveHour.Utilization = 0.80 // exactly at the limit pauses (>=), as the docs say
	if p, ok := decide(s, th, now); !ok || p.Reason != "five-hour" || p.Until != reset5 {
		t.Fatalf("at 80%% of 5h: want pause until the 5h reset, got %+v %v", p, ok)
	}
	s.Weekly.Utilization = 0.95
	if p, _ := decide(s, th, now); p.Reason != "weekly" || p.Until != resetW {
		t.Fatalf("both over: the later reset wins, got %+v", p)
	}
	// automatic resume: after the reset the old utilization no longer counts
	if _, ok := decide(s, th, time.Unix(resetW+1, 0)); ok {
		t.Fatal("after both resets: no pause")
	}
	if p, _ := decide(s, th, time.Unix(reset5+1, 0)); p.Reason != "weekly" {
		t.Fatalf("after the 5h reset the weekly pause stays, got %+v", p)
	}
	if p, _ := decide(s, Thresholds{100, 100}, now); p.Until != 0 {
		t.Fatalf("threshold 100: no pause below 100%%, got %+v", p)
	}
}

func TestApplyRateLimitAndForgery(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	s := &SubscriptionState{}
	s.apply(usageLine{RateLimited: true}, at)
	if s.RateLimited != at.Add(rateLimitPause).Unix() {
		t.Fatalf("rate limit without a reset time pauses for %v", rateLimitPause)
	}
	if p, ok := decide(s, Thresholds{80, 90}, at); !ok || p.Reason != "rate-limited" {
		t.Fatalf("a rate-limited run pauses at once, got %+v", p)
	}
	// a reset far in the future is cut to the window length
	s2 := &SubscriptionState{}
	s2.apply(usageLine{FiveHour: &Window{0.9, at.Add(100 * time.Hour).Unix()}}, at)
	if s2.FiveHour.ResetsAt != at.Add(5*time.Hour).Unix() {
		t.Fatalf("5h reset not capped: %d", s2.FiveHour.ResetsAt)
	}
	// a lower report in the same window does not lift a pause
	s2.apply(usageLine{FiveHour: &Window{0.1, s2.FiveHour.ResetsAt}}, at.Add(time.Minute))
	if s2.FiveHour.Utilization != 0.9 {
		t.Fatalf("utilization went down inside the window: %v", s2.FiveHour.Utilization)
	}
	// review #102 finding 1: a forged report with a slightly later reset must not start a new window (lift the pause)
	runs := s2.Runs
	s2.apply(usageLine{FiveHour: &Window{0, s2.FiveHour.ResetsAt + 1}}, at.Add(90*time.Second))
	if s2.FiveHour.Utilization != 0.9 || s2.Runs != runs+1 {
		t.Fatalf("forged later reset lifted the window: %+v", s2)
	}
	if _, ok := decide(s2, Thresholds{80, 90}, at.Add(2*time.Minute)); !ok {
		t.Fatal("forged line lifted the pause")
	}
	// a real new window: the old one has reset; this resets the token counters
	reset := s2.FiveHour.ResetsAt
	next := time.Unix(reset+1, 0)
	s2.apply(usageLine{InputTokens: 10, FiveHour: &Window{0.2, next.Add(5 * time.Hour).Unix()}}, next)
	if s2.FiveHour.Utilization != 0.2 || s2.InputTokens != 10 || s2.Runs != 1 {
		t.Fatalf("new window after the reset: %+v", s2)
	}
	// also a new window when the reset moves by most of a window length (the report came late)
	s3 := &SubscriptionState{FiveHour: Window{0.95, at.Add(time.Hour).Unix()}}
	s3.apply(usageLine{FiveHour: &Window{0.1, at.Add(5 * time.Hour).Unix()}}, at)
	if s3.FiveHour.Utilization != 0.1 {
		t.Fatalf("reset 4h later (80 %% of the window) is a new window: %+v", s3.FiveHour)
	}
}

func TestParseUsage(t *testing.T) {
	u, at, ok := parseUsage(ts + ` AGENTDEV_USAGE {"job":"issue-1","input_tokens":12,"output_tokens":3,"five_hour":{"utilization":0.5,"resetsAt":1800000000},"seven_day":{"utilization":-1,"resetsAt":1}}`)
	if !ok || u.InputTokens != 12 || u.FiveHour == nil || u.FiveHour.Utilization != 0.5 || u.Weekly != nil || at.IsZero() {
		t.Fatalf("parse: %+v %v", u, ok)
	}
	if _, _, ok := parseUsage(ts + " something else"); ok {
		t.Fatal("other line parsed")
	}
}

// End to end through the fake API: two agents on claude-shared share one budget; a third on its own Secret does not
// pause. A project override pauses only that project. After the reset the pause disappears.
func TestUsagePauseAndResume(t *testing.T) {
	agent := func(name, repo, secret string) *v1.Agent {
		return &v1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1.AgentSpec{Repo: repo, ClaudeSecret: secret}}
	}
	c, cl := collector(t, agent("alice", "o/a", "claude-shared"), agent("bob", "o/b", "claude-shared"), agent("carol", "o/c", "claude-carol"))
	now := time.Unix(1_800_000_000, 0)
	u := &Usage{Client: cl, Direct: cl, NS: ns, Now: func() time.Time { return now }}
	c.Usage = u
	ctx := context.Background()
	reset := now.Add(3 * time.Hour).Unix()
	line := func(util float64) usageLine { return usageLine{FiveHour: &Window{util, reset}} }

	if err := u.Observe(ctx, pod("alice"), line(0.6), now); err != nil {
		t.Fatal(err)
	}
	if got := pauses(t, cl); len(got) != 0 {
		t.Fatalf("60%%: no pause, got %v", got)
	}
	// project o/b lowers its threshold to 50: bob pauses, alice (same subscription, global 80) does not
	settingsCM(t, cl, UsageSettings{Projects: map[string]Thresholds{"o/b": {FiveHour: 50}}})
	if err := u.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pauses(t, cl); len(got) != 1 || got["bob"].Reason != "five-hour" {
		t.Fatalf("override: want bob paused only, got %v", got)
	}
	if err := u.Observe(ctx, pod("bob"), line(0.85), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := pauses(t, cl)
	if len(got) != 2 || got["alice"].Until != reset || got["carol"].Until != 0 {
		t.Fatalf("shared budget: want alice and bob paused, carol not, got %v", got)
	}
	var cm corev1.ConfigMap
	_ = cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: usageCM}, &cm)
	if cm.Data["paused.alice"] == "" || cm.Data["paused.carol"] != "" {
		t.Fatalf("worker keys: %v", cm.Data)
	}
	var evs v1.SecurityEventList
	_ = cl.List(ctx, &evs, client.InNamespace(ns))
	if len(evs.Items) != 2 || evs.Items[0].Spec.Source != "operator" || evs.Items[0].Spec.Rule != "usage-five-hour" {
		t.Fatalf("one security event per new pause, got %d %+v", len(evs.Items), evs.Items)
	}
	if err := u.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = cl.List(ctx, &evs, client.InNamespace(ns))
	if len(evs.Items) != 2 {
		t.Fatalf("no second event for the same pause, got %d", len(evs.Items))
	}
	// the window resets: automatic resume
	now = time.Unix(reset+1, 0)
	if err := u.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pauses(t, cl); len(got) != 0 {
		t.Fatalf("after the reset: no pause, got %v", got)
	}
	// a restarted operator reads the state back
	u2 := &Usage{Client: cl, Direct: cl, NS: ns, Now: func() time.Time { return now }}
	if err := u2.Observe(ctx, pod("carol"), usageLine{RateLimited: true}, now); err != nil {
		t.Fatal(err)
	}
	if got := pauses(t, cl); got["carol"].Reason != "rate-limited" || len(got) != 1 {
		t.Fatalf("rate limit pauses carol only, got %v", got)
	}
}

func pauses(t *testing.T, cl client.Client) map[string]Pause {
	t.Helper()
	var cm corev1.ConfigMap
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: usageCM}, &cm); err != nil {
		return nil
	}
	m := map[string]Pause{}
	_ = json.Unmarshal([]byte(cm.Data["pauses.json"]), &m)
	return m
}

func settingsCM(t *testing.T, cl client.Client, s UsageSettings) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := cl.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageSettingsCM, Namespace: ns},
		Data: map[string]string{"settings.json": string(b)}}); err != nil {
		t.Fatal(err)
	}
}
