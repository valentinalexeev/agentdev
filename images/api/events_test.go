package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

func ev(agent, sev, hash string, t time.Time) securityEvent {
	return securityEvent{Schema: schemaV1, Time: t, Agent: agent, Source: "forge", Rule: "push-branch", Severity: sev,
		Action: "push", Hash: hash}
}

func eventServer(t *testing.T, store eventStore) http.Handler {
	t.Helper()
	c := fake.NewSimpleClientset(sts("alice", 1, 1, nil), sts("bob", 1, 1, nil))
	return routes(fakeVerify, &kubeAgents{c: c, ns: "coding-agents"}, store, nil)
}

type page struct {
	Events     []securityEvent `json:"events"`
	NextCursor string          `json:"next_cursor"`
}

func getPage(t *testing.T, h http.Handler, path string) page {
	t.Helper()
	w := do(t, h, path, "good")
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	var p page
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecurityEventsList(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	now := time.Now().UTC()
	for i, e := range []securityEvent{
		ev("alice", "low", "h1", now.Add(-48*time.Hour)),
		ev("alice", "high", "h2", now.Add(-time.Hour)),
		ev("bob", "medium", "h3", now.Add(-time.Minute)),
		ev("alice", "medium", "h4", now),
	} {
		e := e
		if ok, _ := m.Insert(ctx, &e); !ok || e.ID != int64(i+1) {
			t.Fatalf("insert %d", i)
		}
	}
	dup := ev("alice", "low", "h1", now)
	if ok, _ := m.Insert(ctx, &dup); ok {
		t.Fatal("duplicate hash stored")
	}
	h := eventServer(t, m)

	if w := do(t, h, "/api/security-events", ""); w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if p := getPage(t, h, "/api/security-events"); len(p.Events) != 4 || p.Events[0].Hash != "h4" || p.NextCursor != "" {
		t.Fatalf("all: %+v", p)
	}
	if p := getPage(t, h, "/api/security-events?agent=alice&severity=medium"); len(p.Events) != 1 || p.Events[0].Hash != "h4" {
		t.Fatalf("filter: %+v", p)
	}
	if p := getPage(t, h, "/api/security-events?since=24h"); len(p.Events) != 3 {
		t.Fatalf("since duration: %d", len(p.Events))
	}
	since := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if p := getPage(t, h, "/api/security-events?since="+since); len(p.Events) != 3 {
		t.Fatalf("since time: %d", len(p.Events))
	}
	// paging: 2 + 2, no overlap
	p1 := getPage(t, h, "/api/security-events?limit=2")
	if len(p1.Events) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := getPage(t, h, "/api/security-events?limit=2&cursor="+p1.NextCursor)
	if len(p2.Events) != 2 || p2.NextCursor != "" || p2.Events[0].ID >= p1.Events[1].ID {
		t.Fatalf("page 2: %+v", p2)
	}
	for _, bad := range []string{"severity=critical", "since=yesterday", "limit=0", "limit=501", "cursor=!!", "cursor=" + encodeCursor(0)} {
		if w := do(t, h, "/api/security-events?"+bad, "good"); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	// overview badge: last 7 days
	w := do(t, h, "/api/agents", "good")
	var body struct{ Agents []agent }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	got := map[string]int{}
	for _, a := range body.Agents {
		got[a.Name] = a.SecurityEvents
	}
	if got["alice"] != 3 || got["bob"] != 1 {
		t.Fatalf("counts %v", got)
	}
}

func TestSecurityEventsNoStore(t *testing.T) {
	h := eventServer(t, nil)
	if w := do(t, h, "/api/security-events", "good"); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}

func TestFromSecurityEventObject(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"schema": "agentdev.security/v1", "agent": "alice", "source": "loop", "rule": "git-config", "severity": "medium",
		"action": "core.hooksPath", "time": "2026-10-31T10:00:00Z", "pod": "agent-alice-0", "container": "agent", "hash": "abc"}}}
	e, ok := fromSecurityEventObject(u)
	if !ok || e.Agent != "alice" || e.Job != "" || !e.Time.Equal(time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", e, ok)
	}
	delete(u.Object["spec"].(map[string]any), "hash")
	if _, ok := fromSecurityEventObject(u); ok {
		t.Fatal("accepted an event without hash")
	}
}

func TestSinkSendsOnlyNew(t *testing.T) {
	m := newMemStore()
	wh := newWebhook("http://unused", []byte("k"))
	s := &sink{store: m, out: wh}
	e := ev("alice", "high", "h1", time.Now())
	s.add(context.Background(), e)
	s.add(context.Background(), e) // informer resync or API restart
	if _, ok := wh.next(); !ok {
		t.Fatal("nothing queued")
	}
	if _, ok := wh.next(); ok {
		t.Fatal("queued twice")
	}
}

func TestWebhookFairQueue(t *testing.T) {
	wh := newWebhook("http://unused", []byte("k"))
	wh.PerAgent = 5
	for i := 0; i < 100; i++ { // mallory floods
		wh.Enqueue(ev("mallory", "low", fmt.Sprint("m", i), time.Now()))
	}
	wh.Enqueue(ev("alice", "high", "a1", time.Now()))
	if d := wh.Dropped(); d["mallory"] != 95 || d["alice"] != 0 {
		t.Fatalf("dropped %v", d)
	}
	// round robin: alice's event comes out second, not after mallory's backlog
	var got []string
	for e, ok := wh.next(); ok; e, ok = wh.next() {
		got = append(got, e.Agent)
	}
	if len(got) != 6 || got[1] != "alice" {
		t.Fatalf("order %v", got)
	}
}

func TestUntrustedFromObject(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"agent": "a", "rule": "r", "hash": "h",
		"time": "2026-10-31T10:00:00Z", "source": "loop", "untrusted": true}}}
	if e, ok := fromSecurityEventObject(u); !ok || !e.Untrusted {
		t.Fatalf("%+v", e)
	}
}

func TestWebhookSignedAndRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var gotBody []byte
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
		if calls < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	wh := newWebhook(srv.URL, []byte("s3cret"))
	wh.Now = func() time.Time { return time.Unix(1793480000, 0) }
	var delays []time.Duration
	wh.Sleep = func(_ context.Context, d time.Duration) bool { delays = append(delays, d); return true }
	e := ev("alice", "high", "h1", time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC))
	e.ID = 7
	if err := wh.deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(delays) != 2 || delays[0] != 2*time.Second || delays[1] != 4*time.Second {
		t.Fatalf("calls %d delays %v", calls, delays)
	}
	if gotHdr.Get("X-Agentdev-Timestamp") != "1793480000" || gotHdr.Get("X-Agentdev-Schema") != schemaV1 {
		t.Fatalf("headers %v", gotHdr)
	}
	// receiver-side check, as documented
	if want := sign([]byte("s3cret"), "1793480000", gotBody); gotHdr.Get("X-Agentdev-Signature") != want {
		t.Fatalf("signature %q != %q", gotHdr.Get("X-Agentdev-Signature"), want)
	}
	// format: sha256=<64 hex>
	if got := sign([]byte("key"), "1", []byte("{}")); !strings.HasPrefix(got, "sha256=") || len(got) != 71 {
		t.Fatalf("format %q", got)
	}
	var body securityEvent
	if err := json.Unmarshal(gotBody, &body); err != nil || body.ID != 7 || body.Schema != schemaV1 {
		t.Fatalf("body %s", gotBody)
	}
}

func TestWebhookGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	wh := newWebhook(srv.URL, []byte("k"))
	wh.Attempts = 4
	var delays []time.Duration
	wh.Sleep = func(_ context.Context, d time.Duration) bool { delays = append(delays, d); return true }
	wh.Backoff = 3 * time.Minute
	if err := wh.deliver(context.Background(), ev("a", "low", "h", time.Now())); err == nil {
		t.Fatal("no error after all attempts")
	}
	if len(delays) != 3 || delays[2] != 5*time.Minute {
		t.Fatalf("delays %v (cap 5m)", delays)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	if id, err := decodeCursor(encodeCursor(42)); err != nil || id != 42 {
		t.Fatal(id, err)
	}
}

// TestPgStore runs against a real Postgres when TEST_DATABASE_URL is set (for example a local container).
func TestPgStore(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := newPgStore(ctx, url, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newPgStore(ctx, url, true); err != nil { // migrations are idempotent
		t.Fatal(err)
	}
	h := "test-" + time.Now().Format(time.RFC3339Nano)
	e := ev("pgtest", "high", h, time.Now().UTC())
	if ok, err := s.Insert(ctx, &e); !ok || err != nil || e.ID == 0 {
		t.Fatal(ok, err)
	}
	if ok, err := s.Insert(ctx, &e); ok || err != nil {
		t.Fatal("duplicate", ok, err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE security_events SET action='x' WHERE hash=$1`, h); err == nil {
		t.Fatal("update allowed")
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM security_events WHERE hash=$1`, h); err == nil {
		t.Fatal("delete inside retention allowed")
	}
	l, err := s.List(ctx, eventFilter{Agent: "pgtest", Limit: 10})
	if err != nil || len(l) == 0 || l[0].Hash != h {
		t.Fatal(l, err)
	}
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestPgRoleSeparation needs TEST_DATABASE_URL as a superuser: it sets up the roles like 10-api.sh and checks that the
// API login cannot delete, truncate, drop the trigger or change the retention, but can insert, read and prune.
func TestPgRoleSeparation(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	su, err := newPgStore(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	setup := `DROP TABLE IF EXISTS security_events, security_settings CASCADE;
		DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner') THEN CREATE ROLE agentdev_owner NOLOGIN; END IF;
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api') THEN CREATE ROLE agentdev_api NOLOGIN; END IF; END $$;
		GRANT CREATE, USAGE ON SCHEMA public TO agentdev_owner;
		SET ROLE agentdev_owner;` + schemaSQL + `RESET ROLE;` + grantsSQL
	if _, err := su.db.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}
	conn, err := su.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE agentdev_api`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `RESET ROLE`)
	if _, err := conn.Exec(ctx, `INSERT INTO security_events (schema,time,agent,source,rule,severity,action,hash)
		VALUES ('v1', now(), 'a', 'loop', 'r', 'low', 'x', 'roles-test')`); err != nil {
		t.Fatal("insert:", err)
	}
	for _, q := range []string{`DELETE FROM security_events`, `TRUNCATE security_events`, `UPDATE security_settings SET retention_days=1`,
		`DROP TRIGGER security_events_append_only ON security_events`, `UPDATE security_events SET action='y'`} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("API role may run %q", q)
		}
	}
	if _, err := conn.Exec(ctx, `SELECT security_events_prune()`); err != nil {
		t.Fatal("prune:", err)
	}
}
