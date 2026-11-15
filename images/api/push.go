package main

// Web Push sender (#347). Encryption (RFC 8291, aes128gcm) and VAPID (RFC 8292) come from
// github.com/SherClockHolmes/webpush-go, a maintained library (CLAUDE.md §6: ready-made before custom).
//
// Instance configuration is a NotificationChannel object in the platform namespace (owner decision on #347):
// spec.type webpush, spec.vapid.secretRef (a Secret with publicKey and privateKey, created once by Ansible),
// spec.subject (VAPID contact) and spec.allowedHosts (push services the API may call; the egress policy uses the same
// list). The API reads it every minute; without one Web Push is off and the endpoints answer 404.
//
// Content rule (decision 3 on #340): agent, repository, request type and expiry only. Never text from the model: the
// payload is built from fixed strings and fields that pass the name, repository and enum checks below. The tool name
// and the input of an approval are never sent.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var channelsGVR = schema.GroupVersionResource{Group: "agentdev.dev", Version: "v1alpha1", Resource: "notificationchannels"}

const (
	pushPoll       = 15 * time.Second
	actionTTL      = 30 * time.Minute // an action button works for this long (or until the approval expires)
	pushTTLSeconds = 3600             // the push service keeps an undelivered message this long
	sendsPerWindow = 30               // per user per sendWindow (R-row: notification flood)
	sendWindow     = 10 * time.Minute
)

// snoozeFor: Snooze in a notification re-notifies after this time.
var snoozeFor = 15 * time.Minute

// pushChannel is the effective webpush NotificationChannel.
type pushChannel struct {
	Name         string
	PublicKey    string
	privateKey   string
	Subject      string
	AllowedHosts []string
}

// channelSource reads the NotificationChannel objects and their VAPID Secret.
type channelSource struct {
	k       *kubeAgents
	ns      string
	enabled bool // NOTIFY_CHANNELS contains webpush

	mu  sync.RWMutex
	cur *pushChannel
}

func (c *channelSource) get() *pushChannel {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
}

func (c *channelSource) set(p *pushChannel) {
	c.mu.Lock()
	c.cur = p
	c.mu.Unlock()
}

func (c *channelSource) run(ctx context.Context) {
	for {
		p, err := c.load(ctx)
		if err != nil {
			log.Printf("notification channels: %v", err)
		}
		switch old := c.get(); {
		case p != nil && old == nil:
			log.Printf("web push: on (NotificationChannel %s)", p.Name)
		case p == nil && old != nil:
			log.Printf("web push: off")
		}
		c.set(p)
		if !sleepCtx(ctx, time.Minute) {
			return
		}
	}
}

// load returns the first valid webpush channel (by name), or nil.
func (c *channelSource) load(ctx context.Context) (*pushChannel, error) {
	if !c.enabled || c.k == nil || c.k.d == nil {
		return nil, nil
	}
	list, err := c.k.d.Resource(channelsGVR).Namespace(c.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, u := range list.Items {
		names = append(names, u.GetName())
	}
	slices.Sort(names)
	for _, n := range names {
		for _, u := range list.Items {
			if u.GetName() != n {
				continue
			}
			spec, _ := u.Object["spec"].(map[string]any)
			if t, _ := spec["type"].(string); t != "webpush" {
				continue
			}
			p := &pushChannel{Name: n}
			p.Subject, _ = spec["subject"].(string)
			for _, h := range asStrings(spec["allowedHosts"]) {
				p.AllowedHosts = append(p.AllowedHosts, strings.ToLower(h))
			}
			if len(p.AllowedHosts) == 0 {
				p.AllowedHosts = strings.Split(defaultPushHosts, ",")
			}
			vap, _ := spec["vapid"].(map[string]any)
			ref, _ := vap["secretRef"].(map[string]any)
			sname, _ := ref["name"].(string)
			if sname == "" {
				return nil, fmt.Errorf("NotificationChannel %s: spec.vapid.secretRef.name is required", n)
			}
			sec, err := c.k.c.CoreV1().Secrets(c.ns).Get(ctx, sname, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("NotificationChannel %s: VAPID Secret %s: %w", n, sname, err)
			}
			p.PublicKey, p.privateKey = strings.TrimSpace(string(sec.Data["publicKey"])), strings.TrimSpace(string(sec.Data["privateKey"]))
			if !validKey(p.PublicKey, 65, 0x04) || !validKey(p.privateKey, 32, 0) {
				return nil, fmt.Errorf("NotificationChannel %s: Secret %s needs publicKey (P-256, 65 bytes) and privateKey (32 bytes), base64url", n, sname)
			}
			if !strings.HasPrefix(p.Subject, "mailto:") && !strings.HasPrefix(p.Subject, "https://") {
				return nil, fmt.Errorf("NotificationChannel %s: spec.subject must be mailto: or https://", n)
			}
			return p, nil
		}
	}
	return nil, nil
}

func asStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- payload (the content rule) ----

// pushPayload is the JSON the service worker gets. Every field is fixed text or a checked name.
type pushPayload struct {
	Type    string       `json:"type"`              // approvals | stopped | usage
	Title   string       `json:"title"`             // built here from fixed text, agent and repository
	Body    string       `json:"body"`              // fixed text and the expiry
	Agent   string       `json:"agent"`             // agentNameRe
	Repo    string       `json:"repo"`              // validRepo
	Kind    string       `json:"kind,omitempty"`    // request type: tool, push, apply (from a fixed map)
	Expires string       `json:"expires,omitempty"` // RFC 3339
	URL     string       `json:"url"`               // same-origin path of the console
	Tag     string       `json:"tag"`               // one notification per item (replaces an older one)
	Actions []pushAction `json:"actions,omitempty"` // Deny and Snooze (approvals only); never Approve
}

type pushAction struct {
	Action string `json:"action"` // deny | snooze
	Title  string `json:"title"`
	Token  string `json:"token"`
}

// requestKind maps the approval source (set by the operator, not by the model) to the text of the notification.
var requestKind = map[string]string{"claude": "tool", "push": "push", "executor": "apply"}

func approvalPayload(a approval) (pushPayload, bool) {
	kind, ok := requestKind[a.Source]
	if !ok || !agentNameRe.MatchString(a.Agent) || !validRepo(a.Repo) || !agentNameRe.MatchString(a.Name) {
		return pushPayload{}, false
	}
	p := pushPayload{Type: typeApprovals, Agent: a.Agent, Repo: a.Repo, Kind: kind,
		Title: fmt.Sprintf("%s asks for a %s approval", a.Agent, kind), Body: a.Repo,
		URL: "/#approvals?open=" + url.QueryEscape(a.Name), Tag: "approval:" + a.Name}
	if !a.Expires.IsZero() {
		p.Expires = a.Expires.UTC().Format(time.RFC3339)
		p.Body = fmt.Sprintf("%s · expires %s UTC", a.Repo, a.Expires.UTC().Format("15:04"))
	}
	return p, true
}

func stoppedPayload(a agent, why string) (pushPayload, bool) {
	repo := ""
	if len(a.Repos) > 0 {
		repo = a.Repos[0]
	}
	text := map[string]string{"guardrail": "was paused by a guardrail", "blocked": "is blocked"}[why]
	if text == "" || !agentNameRe.MatchString(a.Name) || (repo != "" && !validRepo(repo)) {
		return pushPayload{}, false
	}
	return pushPayload{Type: typeStopped, Agent: a.Name, Repo: repo, Kind: why, Title: a.Name + " " + text,
		Body: repo, URL: "/#agents?open=" + url.QueryEscape(a.Name), Tag: "stopped:" + a.Name}, true
}

func usagePayload(a agent, until time.Time, reason string) (pushPayload, bool) {
	repo := ""
	if len(a.Repos) > 0 {
		repo = a.Repos[0]
	}
	text := map[string]string{"five-hour": "5-hour limit", "weekly": "weekly limit", "rate-limited": "rate limit"}[reason]
	if text == "" || !agentNameRe.MatchString(a.Name) || (repo != "" && !validRepo(repo)) {
		return pushPayload{}, false
	}
	return pushPayload{Type: typeUsage, Agent: a.Name, Repo: repo, Kind: reason, Expires: until.UTC().Format(time.RFC3339),
		Title: a.Name + " paused: " + text, Body: fmt.Sprintf("%s · until %s UTC", repo, until.UTC().Format("Jan 2 15:04")),
		URL: "/#usage", Tag: "usage:" + a.Name}, true
}

// ---- action tokens ----

// actionClaims bind a token to one user, one approval and its content, one action and a short life. The nonce makes
// it single-use (stored until exp).
type actionClaims struct {
	Sub     string `json:"s"`
	Name    string `json:"n"`
	Hash    string `json:"h"`
	Action  string `json:"a"` // deny | snooze
	Exp     int64  `json:"e"`
	Nonce   string `json:"x"`
	Version int    `json:"v"`
}

// actionKey derives the HMAC key from the VAPID private key: a new VAPID key pair voids every open token.
func actionKey(ch *pushChannel) []byte {
	m := hmac.New(sha256.New, []byte(ch.privateKey))
	m.Write([]byte("agentdev push action token v1"))
	return m.Sum(nil)
}

func signAction(key []byte, c actionClaims) string {
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var errBadToken = errors.New("the action expired or is not valid; open the console")

func verifyAction(key []byte, tok string, at time.Time) (actionClaims, error) {
	var c actionClaims
	body, sig, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 1024 {
		return c, errBadToken
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return c, errBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Version != 1 {
		return c, errBadToken
	}
	if c.Exp <= at.Unix() || (c.Action != "deny" && c.Action != "snooze") || c.Sub == "" || len(c.Nonce) != 32 {
		return c, errBadToken
	}
	return c, nil
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- sender ----

// pushSender delivers one encrypted message; the real one is webpush.SendNotificationWithContext.
type pushSender func(ctx context.Context, msg []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error)

type pusher struct {
	store    notifyStore
	w        *writer
	ar       *approvalReader
	channels func() *pushChannel
	send     pushSender
	client   *http.Client
	limit    *rateLimiter

	mu      sync.Mutex
	seen    map[string]string    // item key -> state already notified (approval name, agent stop, usage pause)
	snoozed map[string]time.Time // subject \x00 approval name -> when to notify again
	primed  bool
}

func newPusher(store notifyStore, w *writer, ar *approvalReader, channels func() *pushChannel) *pusher {
	return &pusher{store: store, w: w, ar: ar, channels: channels, send: webpush.SendNotificationWithContext,
		client: safePushClient(), limit: newRateLimiter(sendsPerWindow, sendWindow),
		seen: map[string]string{}, snoozed: map[string]time.Time{}}
}

// safePushClient refuses private, loopback and link-local addresses after DNS (R-row: endpoint as SSRF vector),
// follows no redirect, and has short timeouts.
func safePushClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !publicIP(ip) {
			return fmt.Errorf("push endpoint resolves to a non-public address %s", host)
		}
		return nil
	}}
	tr := &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second, MaxIdleConns: 10,
		IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 10 * time.Second, Proxy: nil}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.Equal(net.IPv4bcast) || (ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)) // 100.64/10 CGNAT
}

func (p *pusher) run(ctx context.Context) {
	for {
		if p.channels() != nil {
			if err := p.pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("web push: %v", err)
			}
		}
		if !sleepCtx(ctx, pushPoll) {
			return
		}
	}
}

// item is one thing that may notify: the key tells whether it is new, the state whether it changed.
type item struct {
	key, state string
	forge      string
	repos      []string
	payload    pushPayload
	approval   *approval
}

// collect builds the current items: pending approvals, stopped agents (guardrail pause or blocked), usage pauses.
func (p *pusher) collect(ctx context.Context) ([]item, error) {
	var out []item
	lister, ok := p.w.store.(agentLister)
	if !ok {
		return nil, nil
	}
	agents, err := lister.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]agent{}
	for _, a := range agents {
		byName[a.Name] = a
	}
	if p.ar != nil {
		objs, err := p.ar.store.ListApprovals(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range objs {
			a := fromApprovalObject(u)
			if a.State != "pending" || (!a.Expires.IsZero() && a.Expires.Before(now())) {
				continue
			}
			if pl, ok := approvalPayload(a); ok {
				a := a
				out = append(out, item{key: "a:" + a.Name, state: a.ContentHash, forge: a.Forge, repos: []string{a.Repo},
					payload: pl, approval: &a})
			}
		}
	}
	var counts map[string]int
	if p.w.events != nil {
		counts, _ = p.w.events.Counts(ctx, now().Add(-7*24*time.Hour))
	}
	for _, a := range agents {
		why := ""
		switch {
		case a.Paused && counts[a.Name] > 0:
			why = "guardrail" // the same rule as the console (images/ui/src/model.ts)
		case a.Phase == "blocked":
			why = "blocked"
		}
		if why == "" {
			continue
		}
		if pl, ok := stoppedPayload(a, why); ok {
			out = append(out, item{key: "s:" + a.Name, state: why, forge: a.Forge, repos: a.Repos, payload: pl})
		}
	}
	if p.w.usage != nil {
		if st, err := p.w.usage.State(ctx); err == nil {
			var pauses map[string]struct {
				Until  int64  `json:"until"`
				Reason string `json:"reason"`
			}
			if raw, ok := st["pauses"]; ok && json.Unmarshal(raw, &pauses) == nil {
				for name, ps := range pauses {
					a, ok := byName[name]
					until := time.Unix(ps.Until, 0)
					if !ok || !until.After(now()) {
						continue
					}
					if pl, ok := usagePayload(a, until, ps.Reason); ok {
						out = append(out, item{key: "u:" + name, state: strconv.FormatInt(ps.Until, 10), forge: a.Forge,
							repos: a.Repos, payload: pl})
					}
				}
			}
		}
	}
	return out, nil
}

// pass sends what is new since the last pass, and what a snooze has made due. The first pass after a start only
// records the state: a restart does not repeat old notifications.
func (p *pusher) pass(ctx context.Context) error {
	items, err := p.collect(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	fresh := []item{}
	cur := map[string]bool{}
	for _, it := range items {
		cur[it.key] = true
		if p.seen[it.key] != it.state {
			p.seen[it.key] = it.state
			if p.primed {
				fresh = append(fresh, it)
			}
		}
	}
	for k := range p.seen {
		if !cur[k] {
			delete(p.seen, k) // ended: a new stop or pause notifies again
		}
	}
	due := map[string][]string{} // approval key -> subjects whose snooze ended
	for k, t := range p.snoozed {
		sub, name, _ := strings.Cut(k, "\x00")
		if !cur["a:"+name] {
			delete(p.snoozed, k)
			continue
		}
		if !t.After(now()) {
			delete(p.snoozed, k)
			due["a:"+name] = append(due["a:"+name], sub)
		}
	}
	p.primed = true
	p.mu.Unlock()
	if len(fresh) == 0 && len(due) == 0 {
		return nil
	}
	subjects, err := p.store.Subscribers(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subjects {
		u, ok, err := p.store.User(ctx, sub)
		if err != nil || !ok {
			continue
		}
		for _, it := range items {
			if slices.ContainsFunc(fresh, func(f item) bool { return f.key == it.key }) || slices.Contains(due[it.key], sub) {
				p.deliver(ctx, u, it)
			}
		}
	}
	return nil
}

// deliver checks visibility (R35, live), the user's settings and the rate limit, then sends to each device.
func (p *pusher) deliver(ctx context.Context, u userSnap, it item) {
	ch := p.channels()
	if ch == nil {
		return
	}
	id := u.identity()
	visible := p.w.isAdmin(id)
	for _, repo := range it.repos {
		if visible {
			break
		}
		perm, err := p.w.repoPerm(ctx, id, it.forge, repo)
		visible = err == nil && perm >= permWrite
	}
	if !visible {
		return
	}
	s, err := p.store.Settings(ctx, u.Subject)
	if err != nil || !s.wants(it.payload.Type, it.payload.Repo) {
		return
	}
	if !p.limit.allow(u.Subject) {
		log.Printf("web push: rate limit for user %s, dropped %s", u.Subject, it.payload.Tag)
		return
	}
	pl := it.payload
	if it.approval != nil {
		key := actionKey(ch)
		exp := now().Add(actionTTL)
		if !it.approval.Expires.IsZero() && it.approval.Expires.Before(exp) {
			exp = it.approval.Expires
		}
		for _, a := range []struct{ action, title string }{{"deny", "Deny"}, {"snooze", fmt.Sprintf("Snooze %d min", int(snoozeFor/time.Minute))}} {
			pl.Actions = append(pl.Actions, pushAction{Action: a.action, Title: a.title, Token: signAction(key, actionClaims{
				Sub: u.Subject, Name: it.approval.Name, Hash: it.approval.ContentHash, Action: a.action,
				Exp: exp.Unix(), Nonce: newNonce(), Version: 1})})
		}
	}
	msg, _ := json.Marshal(pl)
	subs, err := p.store.Subscriptions(ctx, u.Subject)
	if err != nil {
		return
	}
	for _, s := range subs {
		p.sendOne(ctx, ch, s, msg, pl.Tag)
	}
}

func (p *pusher) sendOne(ctx context.Context, ch *pushChannel, s pushSub, msg []byte, tag string) {
	if validPushEndpoint(s.Endpoint, ch.AllowedHosts) != nil {
		_ = p.store.DeleteEndpoint(ctx, s.Endpoint) // the allowed hosts changed: the subscription is no longer valid
		return
	}
	topic := sha256.Sum256([]byte(tag))
	resp, err := p.send(ctx, msg, &webpush.Subscription{Endpoint: s.Endpoint, Keys: webpush.Keys{P256dh: s.P256dh, Auth: s.Auth}},
		&webpush.Options{HTTPClient: p.client, Subscriber: ch.Subject, VAPIDPublicKey: ch.PublicKey,
			VAPIDPrivateKey: ch.privateKey, TTL: pushTTLSeconds, Urgency: webpush.UrgencyHigh,
			Topic: base64.RawURLEncoding.EncodeToString(topic[:24])})
	if err != nil {
		log.Printf("web push to %s: %v", hostOf(s.Endpoint), err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		_ = p.store.DeleteEndpoint(ctx, s.Endpoint) // RFC 8030 §7.3: the subscription is gone
	case resp.StatusCode >= 300:
		log.Printf("web push to %s: HTTP %d", hostOf(s.Endpoint), resp.StatusCode)
	}
}

// runAction applies a notification button: Deny decides through the same rules as the console (decideAs, via
// "push"); Snooze sends the notification again after snoozeFor. Every token works once.
func (p *pusher) runAction(ctx context.Context, tok string) (int, any) {
	ch := p.channels()
	if ch == nil {
		return http.StatusNotFound, map[string]string{"error": "web push is not enabled on this instance"}
	}
	c, err := verifyAction(actionKey(ch), tok, now())
	if err != nil {
		audit(auditEntry{Action: "push-action", Result: "invalid", Reason: "bad or expired token"})
		return http.StatusUnauthorized, map[string]string{"error": err.Error()}
	}
	first, err := p.store.UseNonce(ctx, c.Nonce, time.Unix(c.Exp, 0).Add(time.Minute))
	if err != nil {
		return http.StatusBadGateway, map[string]string{"error": "cannot check the action"}
	}
	u, ok, _ := p.store.User(ctx, c.Sub)
	who := auditWho{Subject: c.Sub}
	if ok {
		who = p.w.who(u.identity())
	}
	if !first {
		audit(auditEntry{Who: who, Action: "push-action", Result: "denied", Reason: "token used before (replay)",
			Request: map[string]any{"approval": c.Name, "action": c.Action}})
		return http.StatusConflict, map[string]string{"error": "this action was already used"}
	}
	if !ok {
		return http.StatusUnauthorized, map[string]string{"error": errBadToken.Error()}
	}
	switch c.Action {
	case "deny":
		if p.ar == nil {
			return http.StatusNotFound, map[string]string{"error": "approvals are off"}
		}
		return p.ar.decideAs(ctx, u.identity(), c.Name, decideBody{Decision: "deny", ContentHash: c.Hash}, "push")
	default: // snooze
		p.mu.Lock()
		p.snoozed[c.Sub+"\x00"+c.Name] = now().Add(snoozeFor)
		p.mu.Unlock()
		audit(auditEntry{Who: who, Action: "push-snooze", Result: "ok", Request: map[string]any{"approval": c.Name}})
		return http.StatusOK, map[string]any{"state": "snoozed", "minutes": int(snoozeFor / time.Minute)}
	}
}
