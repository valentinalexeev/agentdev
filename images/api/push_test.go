package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Web Push (#347): subscriptions, settings, the content rule, action tokens, removal on 404/410.

func testChannel(t *testing.T) *pushChannel {
	t.Helper()
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return &pushChannel{Name: "webpush", PublicKey: strings.TrimRight(pub, "="), privateKey: strings.TrimRight(priv, "="),
		Subject: "mailto:ops@example.org", AllowedHosts: strings.Split(defaultPushHosts, ",")}
}

func browserKeys(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(auth)
}

type pushEnv struct {
	h     http.Handler
	k     *kubeAgents
	store *memNotify
	p     *pusher
	sent  []sentPush
	code  int // answer of the fake push service
	ch    *pushChannel
	audit *memAudit
}

type sentPush struct {
	endpoint string
	msg      []byte
}

func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	gitea := fakeGitea(t)
	t.Cleanup(gitea.Close)
	k, _ := approvalEnv(t, agentObj("carol", "owner/x", false), approvalObj("carol", aid, "owner/x", t0))
	old := auditDB
	mem := &memAudit{}
	auditDB = mem
	t.Cleanup(func() { auditDB = old })
	oldNow := now
	now = func() time.Time { return t0.Add(5 * time.Minute) }
	t.Cleanup(func() { now = oldNow })
	e := &pushEnv{k: k, store: newMemNotify(), code: http.StatusCreated, ch: testChannel(t), audit: mem}
	wr := &writer{store: k, perms: newGiteaPerms(gitea.URL, "svc"), adminGroup: "agentdev-admins"}
	ar := &approvalReader{store: k, w: wr, agents: k}
	chans := func() *pushChannel { return e.ch }
	e.p = newPusher(e.store, wr, ar, chans)
	e.p.send = func(_ context.Context, msg []byte, s *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		e.sent = append(e.sent, sentPush{s.Endpoint, msg})
		return &http.Response{StatusCode: e.code, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	wr.notify = &notifyAPI{store: e.store, w: wr, channels: chans, limit: newRateLimiter(30, time.Minute), push: e.p}
	e.h = routes(idVerify, k, nil, wr)
	return e
}

func (e *pushEnv) call(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *pushEnv) subscribe(t *testing.T, token, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	p, a := browserKeys(t)
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": p, "auth": a}})
	return e.call(t, "POST", "/api/push/subscriptions", token, string(b))
}

func TestPushSubscriptionValidation(t *testing.T) {
	e := newPushEnv(t)
	for _, c := range []struct {
		ep   string
		want int
	}{
		{"https://fcm.googleapis.com/fcm/send/abc", 201},
		{"https://updates.push.services.mozilla.com/wpush/v2/x", 201},
		{"https://web.push.apple.com/QGx", 201},
		{"http://fcm.googleapis.com/fcm/send/abc", 400},                  // not https
		{"https://evil.example.org/x", 400},                              // host not allowed
		{"https://10.0.0.1/x", 400},                                      // IP literal
		{"https://fcm.googleapis.com:8443/x", 400},                       // port
		{"https://user@fcm.googleapis.com/x", 400},                       // user info
		{"https://fcm.googleapis.com.evil.org/x", 400},                   // suffix trick
		{"https://push.apple.com/x", 400},                                // the wildcard needs a label
		{"https://fcm.googleapis.com/" + strings.Repeat("a", 1100), 400}, // too long
	} {
		if w := e.subscribe(t, "writer", c.ep); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.ep, w.Code, w.Body, c.want)
		}
	}
	// bad keys
	w := e.call(t, "POST", "/api/push/subscriptions", "writer", `{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"AAAA","auth":"AAAA"}}`)
	if w.Code != 400 {
		t.Fatalf("bad keys: %d", w.Code)
	}
	// unauthenticated
	if w := e.subscribe(t, "", "https://fcm.googleapis.com/fcm/send/z"); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestPushSubscriptionCapAndOwnership(t *testing.T) {
	e := newPushEnv(t)
	for i := range maxSubsPerUser {
		if w := e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/"+string(rune('a'+i))); w.Code != 201 {
			t.Fatalf("sub %d: %d", i, w.Code)
		}
	}
	if w := e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/zz"); w.Code != http.StatusConflict {
		t.Fatalf("over cap: %d", w.Code)
	}
	// the same endpoint again is an update, not a new device
	if w := e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/a"); w.Code != 201 {
		t.Fatalf("resubscribe: %d", w.Code)
	}
	// another user cannot remove it
	e.call(t, "DELETE", "/api/push/subscriptions?endpoint=https://fcm.googleapis.com/fcm/send/a", "reader", "")
	if subs, _ := e.store.Subscriptions(context.Background(), "s-w"); len(subs) != maxSubsPerUser {
		t.Fatalf("other user removed a subscription: %d", len(subs))
	}
	e.call(t, "DELETE", "/api/push/subscriptions?endpoint=https://fcm.googleapis.com/fcm/send/a", "writer", "")
	if subs, _ := e.store.Subscriptions(context.Background(), "s-w"); len(subs) != maxSubsPerUser-1 {
		t.Fatalf("own remove: %d", len(subs))
	}
}

func TestNotifySettings(t *testing.T) {
	e := newPushEnv(t)
	w := e.call(t, "GET", "/api/notify/settings", "writer", "")
	var v settingsView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if !v.Types["approvals"] || !v.Types["stopped"] || !v.Types["usage"] || len(v.Projects) != 1 || v.Projects[0] != "owner/x" ||
		len(v.Channels) != 1 || v.PublicKey == "" {
		t.Fatalf("defaults: %+v", v)
	}
	w = e.call(t, "PUT", "/api/notify/settings", "writer", `{"types":{"usage":false},"mutes":[{"project":"owner/x","type":"approvals"}]}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Types["usage"] || len(v.Mutes) != 1 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	// a project the caller cannot see, an unknown type, an unknown field
	for _, b := range []string{
		`{"mutes":[{"project":"owner/y","type":"approvals"}]}`,
		`{"mutes":[{"project":"owner/x","type":"chat"}]}`,
		`{"types":{"chat":true}}`,
		`{"types":{},"extra":1}`,
	} {
		if w := e.call(t, "PUT", "/api/notify/settings", "writer", b); w.Code != 400 {
			t.Errorf("%s: %d", b, w.Code)
		}
	}
	// rita (read only on owner/x) sees no project and cannot mute it
	if w := e.call(t, "PUT", "/api/notify/settings", "reader", `{"mutes":[{"project":"owner/x","type":"*"}]}`); w.Code != 400 {
		t.Fatalf("reader mute: %d", w.Code)
	}
}

func TestNotifyRateLimit(t *testing.T) {
	e := newPushEnv(t)
	e.p.w.notify.limit = newRateLimiter(2, time.Minute)
	for i := range 3 {
		w := e.call(t, "PUT", "/api/notify/settings", "writer", `{}`)
		if want := map[bool]int{true: 200, false: 429}[i < 2]; w.Code != want {
			t.Fatalf("call %d: %d", i, w.Code)
		}
	}
}

// TestPushPayloadHasNoModelText: the content rule (decision 3 on #340). The approval input, the tool name and the
// job hold text the agent wrote; none of it may reach the push service.
func TestPushPayloadHasNoModelText(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	if err := e.p.pass(ctx); err != nil { // first pass: only records the state
		t.Fatal(err)
	}
	e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/w")
	e.subscribe(t, "reader", "https://fcm.googleapis.com/fcm/send/r") // read access only: must get nothing
	// a new approval whose agent-written fields carry a marker
	marker := "IGNORE-PREVIOUS-INSTRUCTIONS"
	obj := approvalObj("carol", "b2c3d4e5b2c3d4e5", "owner/x", t0)
	spec := obj.Object["spec"].(map[string]any)
	spec["input"], spec["tool"], spec["job"] = `{"command":"`+marker+`"}`, "Bash("+marker+")", "issue-"+marker
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	if err := e.p.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 || e.sent[0].endpoint != "https://fcm.googleapis.com/fcm/send/w" {
		t.Fatalf("sent: %+v", e.sent)
	}
	msg := string(e.sent[0].msg)
	for _, bad := range []string{marker, "Bash", "npm", "issue-"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("payload has model text %q: %s", bad, msg)
		}
	}
	var pl pushPayload
	if err := json.Unmarshal(e.sent[0].msg, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Agent != "carol" || pl.Repo != "owner/x" || pl.Kind != "tool" || pl.Expires == "" || pl.URL != "/#approvals?open=carol-b2c3d4e5b2c3d4e5" {
		t.Fatalf("payload: %+v", pl)
	}
	if len(pl.Actions) != 2 || pl.Actions[0].Action != "deny" || pl.Actions[1].Action != "snooze" {
		t.Fatalf("actions (never approve): %+v", pl.Actions)
	}
	// the same state again sends nothing
	if err := e.p.pass(ctx); err != nil || len(e.sent) != 1 {
		t.Fatalf("repeat: %d", len(e.sent))
	}
}

func TestPushMuteAndGone(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	_ = e.p.pass(ctx)
	e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/w")
	e.call(t, "PUT", "/api/notify/settings", "writer", `{"mutes":[{"project":"owner/x","type":"*"}]}`)
	obj := approvalObj("carol", "c3d4e5f6c3d4e5f6", "owner/x", t0)
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	if len(e.sent) != 0 {
		t.Fatalf("muted project notified: %d", len(e.sent))
	}
	// unmute; the push service says 410: the subscription is removed
	e.call(t, "PUT", "/api/notify/settings", "writer", `{}`)
	e.code = http.StatusGone
	obj = approvalObj("carol", "d4e5f6a7d4e5f6a7", "owner/x", t0)
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	if len(e.sent) != 1 {
		t.Fatalf("sent: %d", len(e.sent))
	}
	if subs, _ := e.store.Subscriptions(ctx, "s-w"); len(subs) != 0 {
		t.Fatalf("410 kept the subscription")
	}
}

func lastActions(t *testing.T, e *pushEnv) map[string]string {
	t.Helper()
	var pl pushPayload
	if err := json.Unmarshal(e.sent[len(e.sent)-1].msg, &pl); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, a := range pl.Actions {
		m[a.Action] = a.Token
	}
	return m
}

func TestPushActionTokens(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	_ = e.p.pass(ctx)
	e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/w")
	obj := approvalObj("carol", "e5f6a7b8e5f6a7b8", "owner/x", t0)
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	tok := lastActions(t, e)
	act := func(tk string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"token": tk})
		return e.call(t, "POST", "/api/push/action", "", string(b))
	}
	// tampered: one character of the body changed
	bad := []byte(tok["deny"])
	bad[3] ^= 1
	if w := act(string(bad)); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered: %d", w.Code)
	}
	// snooze works once; a replay is refused
	if w := act(tok["snooze"]); w.Code != 200 {
		t.Fatalf("snooze: %d %s", w.Code, w.Body)
	}
	if w := act(tok["snooze"]); w.Code != http.StatusConflict {
		t.Fatalf("snooze replay: %d", w.Code)
	}
	// after the snooze time the same user is notified again
	n := len(e.sent)
	now = func() time.Time { return t0.Add(5*time.Minute + snoozeFor + time.Second) }
	_ = e.p.pass(ctx)
	if len(e.sent) != n+1 {
		t.Fatalf("snooze did not re-notify: %d -> %d", n, len(e.sent))
	}
	// deny decides the approval (via push), once
	tok = lastActions(t, e)
	if w := act(tok["deny"]); w.Code != 200 {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	u, _ := e.k.GetApproval(ctx, "carol-e5f6a7b8e5f6a7b8")
	if a := fromApprovalObject(u); a.State != "denied" || a.DecidedVia != "push" || a.DecidedBy != "wendy" {
		t.Fatalf("approval: %+v", a)
	}
	if w := act(tok["deny"]); w.Code != http.StatusConflict {
		t.Fatalf("deny replay: %d", w.Code)
	}
	// expired token
	key := actionKey(e.ch)
	old := signAction(key, actionClaims{Sub: "s-w", Name: "carol-" + aid, Hash: strings.Repeat("ab", 32), Action: "deny",
		Exp: now().Add(-time.Second).Unix(), Nonce: newNonce(), Version: 1})
	if w := act(old); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired: %d", w.Code)
	}
	// an approve action is never accepted, even when signed
	ap := signAction(key, actionClaims{Sub: "s-w", Name: "carol-" + aid, Hash: strings.Repeat("ab", 32), Action: "approve",
		Exp: now().Add(time.Minute).Unix(), Nonce: newNonce(), Version: 1})
	if w := act(ap); w.Code != http.StatusUnauthorized {
		t.Fatalf("approve action: %d", w.Code)
	}
	// a token of a user who lost write access does not decide (live permission check in decideAs)
	e.store.SaveUser(ctx, userSnap{Subject: "s-r", Username: "rita", ForgeLogin: "rita"})
	rt := signAction(key, actionClaims{Sub: "s-r", Name: "carol-" + aid, Hash: strings.Repeat("ab", 32), Action: "deny",
		Exp: now().Add(time.Minute).Unix(), Nonce: newNonce(), Version: 1})
	if w := act(rt); w.Code == 200 {
		t.Fatalf("reader decided: %s", w.Body)
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{"142.250.1.1": true, "10.1.2.3": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "::1": false, "fd00::1": false, "2a00:1450::1": true} {
		if got := publicIP(parseIP(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

var metaCreate = metav1.CreateOptions{}

func parseIP(s string) net.IP { return net.ParseIP(s) }

// #404 review: push never uses the snapshot's admin group (only the live forge permission), and a snapshot older than
// snapTTL drives neither delivery nor actions.
func TestPushSnapshotTrust(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	_ = e.p.pass(ctx)
	e.subscribe(t, "group", "https://fcm.googleapis.com/fcm/send/g")  // agentdev-admins, no forge access
	e.subscribe(t, "writer", "https://fcm.googleapis.com/fcm/send/w") // forge write on owner/x
	obj := approvalObj("carol", "f6a7b8c9f6a7b8c9", "owner/x", t0)
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	for _, s := range e.sent {
		if strings.HasSuffix(s.endpoint, "/g") {
			t.Fatalf("admin group from the snapshot got a push")
		}
	}
	if len(e.sent) != 1 {
		t.Fatalf("writer push: %d", len(e.sent))
	}
	tok := lastActions(t, e)
	// a token of the admin-group user (signed for them) does not decide either: no forge access
	key := actionKey(e.ch)
	gt := signAction(key, actionClaims{Sub: "s-g", Name: "carol-f6a7b8c9f6a7b8c9", Hash: strings.Repeat("ab", 32),
		Action: "deny", Exp: now().Add(time.Minute).Unix(), Nonce: newNonce(), Version: 1})
	act := func(tk string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"token": tk})
		return e.call(t, "POST", "/api/push/action", "", string(b))
	}
	if w := act(gt); w.Code == 200 {
		t.Fatalf("admin group decided through push: %s", w.Body)
	}
	// no console visit for longer than snapTTL: no new push, and the writer's token is refused
	base := now()
	now = func() time.Time { return base.Add(snapTTL + time.Minute) }
	n := len(e.sent)
	obj = approvalObj("carol", "a7b8c9d0a7b8c9d0", "owner/x", now())
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	if len(e.sent) != n {
		t.Fatalf("stale snapshot still notified")
	}
	if w := act(tok["deny"]); w.Code != http.StatusUnauthorized {
		t.Fatalf("stale snapshot action: %d %s", w.Code, w.Body)
	}
	// a console visit refreshes it
	e.call(t, "GET", "/api/notify/settings", "writer", "")
	obj = approvalObj("carol", "b8c9d0e1b8c9d0e1", "owner/x", now())
	if _, err := e.k.d.Resource(approvalsGVR).Namespace("coding-agents").Create(ctx, obj, metaCreate); err != nil {
		t.Fatal(err)
	}
	_ = e.p.pass(ctx)
	if len(e.sent) == n {
		t.Fatalf("refreshed snapshot not notified")
	}
}
