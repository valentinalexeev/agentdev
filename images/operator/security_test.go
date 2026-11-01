package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func pod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name + "-0", Namespace: ns, UID: types.UID("pod-" + name),
		Labels: map[string]string{"app": "coding-agent", "agent": name}}}
}

func collector(t *testing.T, objs ...client.Object) (*Collector, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&v1.Agent{}).Build()
	return &Collector{Client: c, Direct: c, NS: ns, Now: time.Now,
		C: SecurityConfig{PauseAfter: 5, Retention: 30 * 24 * time.Hour, MaxPerAgent: 3, Rescan: time.Second}}, c
}

const ts = "2026-10-31T10:00:00.123456789Z"

func line(ts, json string) string { return ts + " AGENTDEV_SECURITY_EVENT " + json }

func TestParseLine(t *testing.T) {
	p := pod("alice")
	// forge sidecar: job left out when empty
	s, ok := parseLine(line(ts, `{"source":"forge","rule":"push-branch","severity":"high","agent":"mallory","action":"push main"}`), p, "forge")
	if !ok || s.Agent != "alice" || s.Job != "" || s.Severity != "high" || s.Schema != v1.SchemaV1 || s.Container != "forge" || len(s.Hash) != 64 {
		t.Fatalf("forge line: %+v %v", s, ok)
	}
	if !s.Time.Equal(&metav1.Time{Time: time.Date(2026, 10, 31, 10, 0, 0, 123456789, time.UTC)}) {
		t.Fatalf("time %v", s.Time)
	}
	// loop: job always present, may be empty; same result as missing
	s2, ok := parseLine(line(ts, `{"source":"loop","rule":"git-config","severity":"medium","agent":"alice","job":"","action":"x"}`), p, "agent")
	if !ok || s2.Job != "" {
		t.Fatalf("loop line: %+v", s2)
	}
	// unknown severity is escalated, not dropped
	s3, _ := parseLine(line(ts, `{"source":"loop","rule":"r","severity":"weird","action":"x"}`), p, "agent")
	if s3.Severity != "high" {
		t.Fatalf("severity %q", s3.Severity)
	}
	for _, l := range []string{
		ts + " plain log line",
		"AGENTDEV_SECURITY_EVENT {\"rule\":\"r\"}", // no timestamp
		line(ts, `{not json`),
		line(ts, `{"source":"loop","severity":"low"}`), // no rule
		line("notatime", `{"rule":"r"}`),
	} {
		if _, ok := parseLine(l, p, "agent"); ok {
			t.Errorf("accepted %q", l)
		}
	}
	// same line twice: same hash; other container or time: other hash
	a, _ := parseLine(line(ts, `{"rule":"r","severity":"low"}`), p, "agent")
	b, _ := parseLine(line(ts, `{"rule":"r","severity":"low"}`), p, "agent")
	c, _ := parseLine(line(ts, `{"rule":"r","severity":"low"}`), p, "forge")
	d, _ := parseLine(line("2026-10-31T10:00:01Z", `{"rule":"r","severity":"low"}`), p, "agent")
	if a.Hash != b.Hash || a.Hash == c.Hash || a.Hash == d.Hash {
		t.Fatal("hash does not dedupe as expected")
	}
}

func events(t *testing.T, c client.Client) []v1.SecurityEvent {
	t.Helper()
	var l v1.SecurityEventList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func getAgent(t *testing.T, c client.Client, name string) v1.Agent {
	t.Helper()
	var a v1.Agent
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConsumeDedupesAndKeepsPosition(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	p := pod("alice")
	log := strings.Join([]string{
		ts + " starting",
		line(ts, `{"source":"loop","rule":"git-config","severity":"medium","agent":"alice","job":"issue-7","action":"core.hooksPath"}`),
		line("2026-10-31T10:00:02Z", `{"source":"loop","rule":"hook","severity":"low","agent":"alice","job":"issue-7","action":"pre-commit"}`),
	}, "\n")
	ctx := context.Background()
	for i := 0; i < 2; i++ { // the second read is a reconnect or an operator restart
		if err := col.consume(ctx, strings.NewReader(log), p, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(events(t, c)); n != 2 {
		t.Fatalf("want 2 events, got %d", n)
	}
	pos, err := col.position(ctx, p, "agent")
	if err != nil || pos == nil || !pos.Time.Equal(time.Date(2026, 10, 31, 10, 0, 2, 0, time.UTC)) {
		t.Fatalf("position %v %v", pos, err)
	}
	if pos, _ := col.position(ctx, p, "forge"); pos != nil {
		t.Fatal("forge container has no position yet")
	}
	if a := getAgent(t, c, "alice"); a.Spec.Paused {
		t.Fatal("paused on low/medium events")
	}
}

func TestFollowUsesPosition(t *testing.T) {
	col, _ := collector(t, agent("alice", nil))
	p := pod("alice")
	ctx := context.Background()
	var got []*metav1.Time
	col.Logs = func(_ context.Context, _, _, _ string, since *metav1.Time) (io.ReadCloser, error) {
		got = append(got, since)
		return io.NopCloser(strings.NewReader(line(ts, `{"rule":"r","severity":"low"}`))), nil
	}
	col.streams = map[string]bool{"k": true}
	col.follow(ctx, p, "agent", "k")
	col.follow(ctx, p, "agent", "k")
	if got[0] != nil || got[1] == nil || got[1].Format(time.RFC3339Nano) != ts {
		t.Fatalf("since: %v", got)
	}
	if col.streams["k"] {
		t.Fatal("stream key not released")
	}
}

func TestPauseOnHighSeverity(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	p := pod("alice")
	spec, _ := parseLine(line(ts, `{"source":"forge","rule":"push-branch","severity":"high","action":"push to main"}`), p, "forge")
	if err := col.Record(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	a := getAgent(t, c, "alice")
	cond := meta.FindStatusCondition(a.Status.Conditions, "SecurityPaused")
	if !a.Spec.Paused || cond == nil || cond.Reason != "HighSeverity" || !strings.Contains(cond.Message, "push-branch") {
		t.Fatalf("not paused: %+v %+v", a.Spec, cond)
	}
	// reconciler: resuming keeps the reason but marks it no longer in force
	r := &AgentReconciler{Client: c, D: Defaults{WorkerImage: "w", ForgeImage: "f"}}
	a.Spec.Paused = false
	if err := c.Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "alice")
	a = getAgent(t, c, "alice")
	if cond := meta.FindStatusCondition(a.Status.Conditions, "SecurityPaused"); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Resumed" {
		t.Fatalf("after resume: %+v", cond)
	}
}

func TestPauseAfterTooManyInRun(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	ctx := context.Background()
	rec := func(p *corev1.Pod, i int, job string) {
		t.Helper()
		ts := time.Date(2026, 10, 31, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano)
		spec, _ := parseLine(line(ts, `{"rule":"r","severity":"low","job":"`+job+`","action":"x"}`), p, "agent")
		if err := col.Record(ctx, p, spec); err != nil {
			t.Fatal(err)
		}
	}
	old, cur := pod("alice"), pod("alice")
	old.UID = "pod-alice-old"
	for i := 0; i < 5; i++ {
		rec(old, i, "issue-1") // an earlier run of the same job (other pod UID) does not count
	}
	for i := 10; i < 15; i++ {
		rec(cur, i, "issue-1")
	}
	for i := 20; i < 25; i++ {
		rec(cur, i, "") // events without a job share one per-pod bucket
	}
	if getAgent(t, c, "alice").Spec.Paused {
		t.Fatal("paused at the limit")
	}
	rec(cur, 30, "issue-1") // 6th event of this run of issue-1, counted although the cache may lag
	a := getAgent(t, c, "alice")
	cond := meta.FindStatusCondition(a.Status.Conditions, "SecurityPaused")
	if !a.Spec.Paused || cond == nil || cond.Reason != "TooManyEvents" || !strings.Contains(cond.Message, "6 security events") {
		t.Fatalf("not paused: %+v", cond)
	}
}

func TestPauseAfterTooManyWithoutJob(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	p := pod("alice")
	for i := 0; i < 6; i++ {
		ts := time.Date(2026, 10, 31, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano)
		spec, _ := parseLine(line(ts, `{"rule":"r","severity":"low"}`), p, "forge")
		if err := col.Record(context.Background(), p, spec); err != nil {
			t.Fatal(err)
		}
	}
	if !getAgent(t, c, "alice").Spec.Paused {
		t.Fatal("events without a job never pause")
	}
}

func TestSourceFromContainer(t *testing.T) {
	p := pod("alice")
	// Claude prints a line in the agent container that claims to be the forge sidecar
	s, ok := parseLine(line(ts, `{"source":"forge","rule":"push-branch","severity":"high","action":"x"}`), p, "agent")
	if !ok || s.Source != "loop" || !s.Untrusted {
		t.Fatalf("forged source accepted: %+v", s)
	}
	s, _ = parseLine(line(ts, `{"source":"loop","rule":"r","severity":"low"}`), p, "forge")
	if s.Source != "forge" || s.Untrusted {
		t.Fatalf("forge container: %+v", s)
	}
}

func TestJobValidation(t *testing.T) {
	p := pod("alice")
	for job, want := range map[string]string{"issue-7": "issue-7", "pr-12": "pr-12", "": "", "issue-": "",
		"issue-7x": "", "../etc": "", strings.Repeat("a", 2000): "", "pr-1234567890123": ""} {
		s, _ := parseLine(line(ts, `{"rule":"r","severity":"low","job":"`+job+`"}`), p, "agent")
		if s.Job != want {
			t.Errorf("job %.20q: got %q want %q", job, s.Job, want)
		}
	}
}

func TestClipRuneSafe(t *testing.T) {
	s := strings.Repeat("a", 79) + "ü" // ü is 2 bytes: a byte cut at 80 would split it
	if got := clip(s, 80); got != strings.Repeat("a", 79) || !utf8.ValidString(got) {
		t.Fatalf("clip %q", got)
	}
	if got := clip("€€", 4); got != "€" {
		t.Fatalf("clip %q", got)
	}
	if !utf8.ValidString(clip("a\xffb", 10)) {
		t.Fatal("invalid bytes kept")
	}
	spec, _ := parseLine(line(ts, `{"rule":"`+s+`","severity":"low","action":"`+strings.Repeat("ж", 400)+`"}`), pod("a"), "agent")
	if !utf8.ValidString(spec.Rule) || !utf8.ValidString(spec.Action) || len(spec.Action) > 503 {
		t.Fatalf("parsed fields not valid UTF-8: %d", len(spec.Action))
	}
}

// failingCreate fails SecurityEvent creation for one rule.
type failingCreate struct {
	client.Client
	rule string
}

func (f failingCreate) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if e, ok := o.(*v1.SecurityEvent); ok && e.Spec.Rule == f.rule {
		return io.ErrUnexpectedEOF
	}
	return f.Client.Create(ctx, o, opts...)
}

func TestPositionNotAdvancedOnFailure(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	p := pod("alice")
	ctx := context.Background()
	log := strings.Join([]string{
		line("2026-10-31T10:00:01Z", `{"rule":"ok1","severity":"low"}`),
		line("2026-10-31T10:00:02Z", `{"rule":"bad","severity":"low"}`),
		line("2026-10-31T10:00:03Z", `{"rule":"ok2","severity":"low"}`),
	}, "\n")
	col.Client = failingCreate{Client: c, rule: "bad"}
	if err := col.consume(ctx, strings.NewReader(log), p, "agent"); err == nil {
		t.Fatal("want an error on the failed record")
	}
	pos, _ := col.position(ctx, p, "agent")
	if pos == nil || !pos.Time.Equal(time.Date(2026, 10, 31, 10, 0, 1, 0, time.UTC)) {
		t.Fatalf("position moved past the failed line: %v", pos)
	}
	col.Client = c // reconnect: the failed line and the rest are recorded
	if err := col.consume(ctx, strings.NewReader(log), p, "agent"); err != nil {
		t.Fatal(err)
	}
	if n := len(events(t, c)); n != 3 {
		t.Fatalf("want 3 events, got %d", n)
	}
}

func TestFloodRateLimited(t *testing.T) {
	col, c := collector(t, agent("mallory", nil), agent("alice", nil))
	col.C.RatePerMin, col.C.RateBurst = 10, 20
	col.C.PauseAfter = 1000
	ctx := context.Background()
	m, a := pod("mallory"), pod("alice")
	var flood []string
	for i := 0; i < 200; i++ { // 200 lines within 2 seconds
		ts := time.Date(2026, 10, 31, 10, 0, 0, i*10_000_000, time.UTC).Format(time.RFC3339Nano)
		flood = append(flood, line(ts, `{"rule":"r","severity":"low"}`))
	}
	if err := col.consume(ctx, strings.NewReader(strings.Join(flood, "\n")), m, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := col.consume(ctx, strings.NewReader(line(ts, `{"rule":"real","severity":"low"}`)), a, "forge"); err != nil {
		t.Fatal(err)
	}
	var mal, ali, limited int
	for _, e := range events(t, c) {
		switch e.Spec.Agent {
		case "mallory":
			mal++
			if e.Spec.Rule == "rate-limited" {
				limited++
			}
		case "alice":
			ali++
		}
	}
	if mal != 21 || limited != 1 || ali != 1 {
		t.Fatalf("mallory %d (rate-limited %d), alice %d", mal, limited, ali)
	}
	if col.Dropped.Load() != 180 {
		t.Fatalf("dropped %d", col.Dropped.Load())
	}
	if !getAgent(t, c, "mallory").Spec.Paused || getAgent(t, c, "alice").Spec.Paused {
		t.Fatal("the flood pauses only its own agent")
	}
	// the bucket refills: a minute later the agent's lines are recorded again
	late := line("2026-10-31T10:01:30Z", `{"rule":"later","severity":"low"}`)
	if ok, _ := col.allow(string(m.UID)+"/agent", time.Date(2026, 10, 31, 10, 1, 30, 0, time.UTC)); !ok {
		t.Fatal("bucket did not refill")
	}
	_ = late
}

func TestPauseUnknownAgentIsNoError(t *testing.T) {
	col, _ := collector(t)
	p := pod("legacy")
	spec, _ := parseLine(line(ts, `{"rule":"r","severity":"high"}`), p, "agent")
	if err := col.Record(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	mk := func(agent string, age time.Duration, i int) *v1.SecurityEvent {
		s := v1.SecurityEventSpec{Agent: agent, Time: metav1.NewTime(now.Add(-age)), Hash: string(rune('a'+i)) + strings.Repeat("0", 63)}
		return &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(s), Namespace: ns}, Spec: s}
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: positionsCM, Namespace: ns},
		Data: map[string]string{"pod-alice.agent": ts, "gone.agent": ts}}
	running := pod("alice")
	col, c := collector(t, running, cm,
		mk("alice", 31*24*time.Hour, 0),                                                                                  // too old
		mk("alice", time.Hour, 1), mk("alice", 2*time.Hour, 2), mk("alice", 3*time.Hour, 3), mk("alice", 4*time.Hour, 4), // cap 3
		mk("bob", 40*24*time.Hour, 5), mk("bob", time.Minute, 6))
	col.Now = func() time.Time { return now }
	if err := col.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	left := map[string]int{}
	for _, e := range events(t, c) {
		left[e.Spec.Agent]++
		if e.Spec.Agent == "alice" && e.Spec.Time.Time.Before(now.Add(-3*time.Hour-time.Second)) {
			t.Errorf("kept an old alice event %v", e.Spec.Time)
		}
	}
	if left["alice"] != 3 || left["bob"] != 1 {
		t.Fatalf("left %v", left)
	}
	var got corev1.ConfigMap
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: positionsCM}, &got)
	if _, ok := got.Data["gone.agent"]; ok || got.Data["pod-alice.agent"] == "" {
		t.Fatalf("positions %v", got.Data)
	}
}

// A failed Record makes the stream re-read its lines; the re-read must not take tokens again, or a long DB/API
// outage would end in a false rate-limited pause.
func TestRetryDoesNotDrainBucket(t *testing.T) {
	col, c := collector(t, agent("alice", nil))
	col.C.RatePerMin, col.C.RateBurst = 10, 2
	col.C.PauseAfter = 1000
	p := pod("alice")
	ctx := context.Background()
	log := strings.Join([]string{
		line("2026-10-31T10:00:01Z", `{"rule":"ok1","severity":"low"}`),
		line("2026-10-31T10:00:01.5Z", `{"rule":"bad","severity":"low"}`),
	}, "\n")
	col.Client = failingCreate{Client: c, rule: "bad"}
	for i := 0; i < 50; i++ { // a long outage: every reconnect re-reads the same lines
		if err := col.consume(ctx, strings.NewReader(log), p, "agent"); err == nil {
			t.Fatal("want an error on the failed record")
		}
	}
	col.Client = c
	if err := col.consume(ctx, strings.NewReader(log), p, "agent"); err != nil {
		t.Fatal(err)
	}
	for _, e := range events(t, c) {
		if e.Spec.Rule == "rate-limited" {
			t.Fatal("retries were charged as a flood")
		}
	}
	if n := len(events(t, c)); n != 2 || getAgent(t, c, "alice").Spec.Paused || col.Dropped.Load() != 0 {
		t.Fatalf("events %d, dropped %d", n, col.Dropped.Load())
	}
}

func TestPruneDropsBucketsOfGonePods(t *testing.T) {
	running := pod("alice")
	col, _ := collector(t, running)
	now := time.Now()
	col.allow(string(running.UID)+"/agent", now)
	col.allow("gone-uid/agent", now)
	if err := col.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := col.buckets["gone-uid/agent"]; ok {
		t.Fatal("bucket of a gone pod kept")
	}
	if _, ok := col.buckets[string(running.UID)+"/agent"]; !ok {
		t.Fatal("bucket of a running pod dropped")
	}
}

// #65: a refusal printed by the egress proxy becomes an event of the agent whose pod made the request.
func TestEgressRefusalBelongsToAgent(t *testing.T) {
	alice := pod("alice")
	alice.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.7"}
	bob := pod("bob")
	bob.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.8"}
	proxy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "egress-proxy-abc", Namespace: ns, UID: "pod-proxy",
		Labels: map[string]string{"app": egressApp}}}
	col, c := collector(t, agent("alice", nil), agent("bob", nil), alice, bob, proxy)
	log := strings.Join([]string{
		ts + " 2026/10/31 10:00:00| Starting Squid",
		line(ts, `{"rule":"egress-host","severity":"medium","client":"10.244.0.7","action":"CONNECT evil.example:443"}`),
		line("2026-10-31T10:00:01Z", `{"rule":"egress-host","severity":"medium","client":"10.9.9.9","action":"CONNECT x:443"}`),
	}, "\n")
	if err := col.consume(context.Background(), strings.NewReader(log), proxy, "squid"); err != nil {
		t.Fatal(err)
	}
	ev := events(t, c)
	if len(ev) != 1 {
		t.Fatalf("want 1 event (unknown client dropped), got %d", len(ev))
	}
	e := ev[0].Spec
	if e.Agent != "alice" || e.Source != "egress" || e.Untrusted || e.Pod != alice.Name || e.Container != egressApp ||
		e.Rule != "egress-host" || e.Action != "CONNECT evil.example:443" {
		t.Fatalf("event %+v", e)
	}
	if ev[0].Labels["agentdev/pod-uid"] != string(alice.UID) {
		t.Fatal("the event must count against the agent's run, not the proxy")
	}
	if pos, _ := col.position(context.Background(), proxy, "squid"); pos == nil {
		t.Fatal("position of the proxy not saved")
	}
}
