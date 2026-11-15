package main

// Notifications for each user (#347, docs/console-ux.md §4, decision 3 on #340). This file holds the per-user data and
// its HTTP handlers; push.go sends. Owner decision on #347: the instance-level part is a NotificationChannel object
// (cluster admin: type, VAPID key reference, allowed push hosts), the per-user part lives here in the API database,
// edited by the user in the console Settings at runtime:
//
//   - push subscriptions (POST/DELETE /api/push/subscriptions): one per browser, max. maxSubsPerUser per user;
//   - settings (GET/PUT /api/notify/settings): one switch per notification type, and mutes per (project, type);
//   - an identity snapshot (subject, forge logins, groups), refreshed on every call of these endpoints. The sender
//     has no user token, so it checks visibility with this snapshot and the live forge permission (R35, fail closed).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Notification types (#347). "*" in a mute means every type of that project.
const (
	typeApprovals = "approvals"
	typeStopped   = "stopped"
	typeUsage     = "usage"
	typeAll       = "*"
)

var notifyTypes = []string{typeApprovals, typeStopped, typeUsage}

const (
	maxSubsPerUser   = 10
	maxEndpointLen   = 1024
	maxMutesPerUser  = 500
	maxBodyNotify    = 64 << 10
	defaultPushHosts = "fcm.googleapis.com,updates.push.services.mozilla.com,*.push.apple.com,*.notify.windows.com"
)

type pushSub struct {
	Subject  string    `json:"-"`
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"-"`
	Auth     string    `json:"-"`
	Created  time.Time `json:"created"`
}

type notifyMute struct {
	Project string `json:"project"`
	Type    string `json:"type"` // a notification type, or "*"
}

// notifySettings of one user. Types: absent = on (the default of #347: on for the user's projects).
type notifySettings struct {
	Types map[string]bool `json:"types"`
	Mutes []notifyMute    `json:"mutes"`
}

// wants: the user gets a notification of type typ for project.
func (s notifySettings) wants(typ, project string) bool {
	if on, ok := s.Types[typ]; ok && !on {
		return false
	}
	for _, m := range s.Mutes {
		if m.Project == project && (m.Type == typ || m.Type == typeAll) {
			return false
		}
	}
	return true
}

// userSnap is what the sender knows of a user (no token, no secret).
type userSnap struct {
	Subject          string   `json:"sub"`
	Username         string   `json:"preferred_username"`
	ForgeLogin       string   `json:"forge_login,omitempty"`
	ForgeLoginGitHub string   `json:"forge_login_github,omitempty"`
	ForgeLoginGitea  string   `json:"forge_login_gitea,omitempty"`
	Groups           []string `json:"groups,omitempty"`
	// Updated is when the user last called a notify endpoint with a valid token (set by the store, not stored in
	// the JSON). Push and its actions trust the snapshot only for snapTTL after that (#404 review).
	Updated time.Time `json:"-"`
}

// snapTTL bounds how long a snapshot (forge logins) may drive push delivery and Deny/Snooze after the user's last
// console visit. The admin group of a snapshot is never used for push (pushIdentity): a removed admin must not keep
// the admin bypass. After snapTTL the user gets no push until the next console visit (fail closed).
const snapTTL = 24 * time.Hour

func (u userSnap) fresh(at time.Time) bool {
	return !u.Updated.IsZero() && at.Sub(u.Updated) <= snapTTL
}

// pushIdentity is the identity for push delivery and actions: forge logins only, no groups, so only the live forge
// permission counts (R35), never the snapshot's agentdev-admins membership.
func (u userSnap) pushIdentity() *identity {
	id := u.identity()
	id.Groups = nil
	return id
}

func snapOf(id *identity) userSnap {
	return userSnap{Subject: id.Subject, Username: id.Username, ForgeLogin: id.ForgeLogin,
		ForgeLoginGitHub: id.ForgeLoginGitHub, ForgeLoginGitea: id.ForgeLoginGitea, Groups: id.Groups}
}

func (u userSnap) identity() *identity {
	return &identity{Subject: u.Subject, Username: u.Username, ForgeLogin: u.ForgeLogin,
		ForgeLoginGitHub: u.ForgeLoginGitHub, ForgeLoginGitea: u.ForgeLoginGitea, Groups: u.Groups}
}

// notifyStore keeps the per-user notification data (Postgres in a deployment, memory in dev and tests).
type notifyStore interface {
	SaveUser(ctx context.Context, u userSnap) error
	User(ctx context.Context, subject string) (userSnap, bool, error)
	AddSubscription(ctx context.Context, s pushSub, max int) error // errTooMany over max
	DeleteSubscription(ctx context.Context, subject, endpoint string) (bool, error)
	DeleteEndpoint(ctx context.Context, endpoint string) error // the push service said 404 or 410
	Subscriptions(ctx context.Context, subject string) ([]pushSub, error)
	Subscribers(ctx context.Context) ([]string, error) // subjects with at least one subscription
	Settings(ctx context.Context, subject string) (notifySettings, error)
	SaveSettings(ctx context.Context, subject string, s notifySettings) error
	// UseNonce records an action token nonce until exp; false when it was used before (replay).
	UseNonce(ctx context.Context, nonce string, exp time.Time) (bool, error)
}

var errTooMany = errors.New("too many push subscriptions")

// ---- memory store (dev, tests) ----

type memNotify struct {
	mu       sync.Mutex
	users    map[string]userSnap
	subs     map[string]pushSub // by endpoint
	settings map[string]notifySettings
	nonces   map[string]time.Time
}

func newMemNotify() *memNotify {
	return &memNotify{users: map[string]userSnap{}, subs: map[string]pushSub{}, settings: map[string]notifySettings{},
		nonces: map[string]time.Time{}}
}

func (m *memNotify) SaveUser(_ context.Context, u userSnap) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u.Updated = now()
	m.users[u.Subject] = u
	return nil
}

func (m *memNotify) User(_ context.Context, sub string) (userSnap, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[sub]
	return u, ok, nil
}

func (m *memNotify) AddSubscription(_ context.Context, s pushSub, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.subs[s.Endpoint]; ok && old.Subject != s.Subject {
		delete(m.subs, s.Endpoint) // the browser moved to another user: the newest sign-in owns it
	}
	n := 0
	for _, x := range m.subs {
		if x.Subject == s.Subject && x.Endpoint != s.Endpoint {
			n++
		}
	}
	if n >= max {
		return errTooMany
	}
	m.subs[s.Endpoint] = s
	return nil
}

func (m *memNotify) DeleteSubscription(_ context.Context, sub, endpoint string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.subs[endpoint]; ok && s.Subject == sub {
		delete(m.subs, endpoint)
		return true, nil
	}
	return false, nil
}

func (m *memNotify) DeleteEndpoint(_ context.Context, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, endpoint)
	return nil
}

func (m *memNotify) Subscriptions(_ context.Context, sub string) ([]pushSub, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pushSub
	for _, s := range m.subs {
		if s.Subject == sub {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out, nil
}

func (m *memNotify) Subscribers(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, s := range m.subs {
		if !seen[s.Subject] {
			seen[s.Subject] = true
			out = append(out, s.Subject)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memNotify) Settings(_ context.Context, sub string) (notifySettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings[sub]
	if s.Types == nil {
		s.Types = map[string]bool{}
	}
	return s, nil
}

func (m *memNotify) SaveSettings(_ context.Context, sub string, s notifySettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[sub] = s
	return nil
}

func (m *memNotify) UseNonce(_ context.Context, nonce string, exp time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := now()
	for k, e := range m.nonces {
		if e.Before(t) {
			delete(m.nonces, k)
		}
	}
	if _, ok := m.nonces[nonce]; ok {
		return false, nil
	}
	m.nonces[nonce] = exp
	return true, nil
}

// ---- Postgres store (tables in schema.sql, rights in grants.sql) ----

func (s *pgStore) SaveUser(ctx context.Context, u userSnap) error {
	raw, _ := json.Marshal(u)
	_, err := s.db.Exec(ctx, `INSERT INTO notify_users (subject, snapshot, updated) VALUES ($1,$2,now())
		ON CONFLICT (subject) DO UPDATE SET snapshot = EXCLUDED.snapshot, updated = now()`, u.Subject, raw)
	return err
}

func (s *pgStore) User(ctx context.Context, sub string) (userSnap, bool, error) {
	var raw []byte
	var updated time.Time
	err := s.db.QueryRow(ctx, `SELECT snapshot, updated FROM notify_users WHERE subject = $1`, sub).Scan(&raw, &updated)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return userSnap{}, false, nil
		}
		return userSnap{}, false, err
	}
	var u userSnap
	if json.Unmarshal(raw, &u) != nil {
		return userSnap{}, false, nil
	}
	u.Updated = updated
	return u, true, nil
}

func (s *pgStore) AddSubscription(ctx context.Context, p pushSub, max int) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// one transaction per user: the cap holds also with two API replicas
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('push:' || $1))`, p.Subject); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1 AND subject <> $2`, p.Endpoint, p.Subject); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE subject = $1 AND endpoint <> $2`,
		p.Subject, p.Endpoint).Scan(&n); err != nil {
		return err
	}
	if n >= max {
		return errTooMany
	}
	if _, err := tx.Exec(ctx, `INSERT INTO push_subscriptions (subject, endpoint, p256dh, auth) VALUES ($1,$2,$3,$4)
		ON CONFLICT (endpoint) DO UPDATE SET p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth`,
		p.Subject, p.Endpoint, p.P256dh, p.Auth); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *pgStore) DeleteSubscription(ctx context.Context, sub, endpoint string) (bool, error) {
	t, err := s.db.Exec(ctx, `DELETE FROM push_subscriptions WHERE subject = $1 AND endpoint = $2`, sub, endpoint)
	return err == nil && t.RowsAffected() > 0, err
}

func (s *pgStore) DeleteEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	return err
}

func (s *pgStore) Subscriptions(ctx context.Context, sub string) ([]pushSub, error) {
	rows, err := s.db.Query(ctx, `SELECT endpoint, p256dh, auth, created FROM push_subscriptions WHERE subject = $1 ORDER BY endpoint`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushSub
	for rows.Next() {
		p := pushSub{Subject: sub}
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *pgStore) Subscribers(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT subject FROM push_subscriptions ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *pgStore) Settings(ctx context.Context, sub string) (notifySettings, error) {
	out := notifySettings{Types: map[string]bool{}}
	rows, err := s.db.Query(ctx, `SELECT project, type, enabled FROM notify_settings WHERE subject = $1 ORDER BY project, type`, sub)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var project, typ string
		var on bool
		if err := rows.Scan(&project, &typ, &on); err != nil {
			return out, err
		}
		if project == "" {
			out.Types[typ] = on
		} else if !on {
			out.Mutes = append(out.Mutes, notifyMute{Project: project, Type: typ})
		}
	}
	return out, rows.Err()
}

func (s *pgStore) SaveSettings(ctx context.Context, sub string, n notifySettings) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM notify_settings WHERE subject = $1`, sub); err != nil {
		return err
	}
	for typ, on := range n.Types {
		if _, err := tx.Exec(ctx, `INSERT INTO notify_settings (subject, project, type, enabled) VALUES ($1,'',$2,$3)`, sub, typ, on); err != nil {
			return err
		}
	}
	for _, m := range n.Mutes {
		if _, err := tx.Exec(ctx, `INSERT INTO notify_settings (subject, project, type, enabled) VALUES ($1,$2,$3,false)
			ON CONFLICT DO NOTHING`, sub, m.Project, m.Type); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *pgStore) UseNonce(ctx context.Context, nonce string, exp time.Time) (bool, error) {
	_, _ = s.db.Exec(ctx, `DELETE FROM push_action_nonces WHERE expires < now()`)
	t, err := s.db.Exec(ctx, `INSERT INTO push_action_nonces (nonce, expires) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nonce, exp)
	if err != nil {
		return false, err
	}
	return t.RowsAffected() == 1, nil
}

// ---- validation ----

// validPushEndpoint: https on port 443, a host on the allowed list of the NotificationChannel, no user info, no IP
// literal (R-row "push endpoint as SSRF vector", docs/security.md). The sender checks the resolved address again.
func validPushEndpoint(raw string, allowed []string) error {
	if len(raw) > maxEndpointLen {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Fragment != "" {
		return errors.New("endpoint must be an https URL")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("endpoint must use port 443")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return errors.New("endpoint host must be a name")
	}
	if !hostAllowed(host, allowed) {
		return errors.New("endpoint host is not an allowed push service")
	}
	return nil
}

// hostAllowed: an exact name, or "*.suffix" for any name below suffix (one or more levels).
func hostAllowed(host string, allowed []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if suf, ok := strings.CutPrefix(a, "*."); ok {
			if strings.HasSuffix(host, "."+suf) {
				return true
			}
		} else if a != "" && host == a {
			return true
		}
	}
	return false
}

// validKey: base64url (padding optional) of exactly n bytes; first, when not 0, the first byte.
func validKey(s string, n int, first byte) bool {
	if len(s) > 200 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	return err == nil && len(b) == n && (first == 0 || b[0] == first)
}

// ---- rate limit (per user, per purpose) ----

type rateLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := now()
	keep := l.hits[key][:0]
	for _, h := range l.hits[key] {
		if t.Sub(h) < l.window {
			keep = append(keep, h)
		}
	}
	if len(keep) >= l.n {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, t)
	return true
}

// ---- handlers ----

type notifyAPI struct {
	store    notifyStore
	w        *writer
	channels func() *pushChannel // nil result: Web Push is off on this instance
	limit    *rateLimiter        // writes per user
	push     *pusher             // actions (push.go); nil in tests without a channel
}

func (n *notifyAPI) caller(r *http.Request) *identity {
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	return id
}

// touch refreshes the user's snapshot (forge logins and groups change in the IdP) and applies the write rate limit.
func (n *notifyAPI) touch(rw http.ResponseWriter, r *http.Request, write bool) (*identity, bool) {
	id := n.caller(r)
	if id == nil || id.Subject == "" {
		writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return nil, false
	}
	if write && !n.limit.allow(id.Subject) {
		writeJSON(rw, http.StatusTooManyRequests, map[string]string{"error": "too many changes; try again in a minute"})
		return nil, false
	}
	if err := n.store.SaveUser(r.Context(), snapOf(id)); err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot store the settings"})
		return nil, false
	}
	return id, true
}

// GET /api/push/vapid-public-key: the application server key for PushManager.subscribe. 404 when Web Push is off.
func (n *notifyAPI) vapidKey(rw http.ResponseWriter, r *http.Request) {
	ch := n.channels()
	if ch == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "web push is not enabled on this instance"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"publicKey": ch.PublicKey})
}

type subscribeBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// POST /api/push/subscriptions: the PushSubscription.toJSON() of this browser.
func (n *notifyAPI) subscribe(rw http.ResponseWriter, r *http.Request) {
	ch := n.channels()
	if ch == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "web push is not enabled on this instance"})
		return
	}
	id, ok := n.touch(rw, r, true)
	if !ok {
		return
	}
	var b subscribeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "body: {endpoint, keys: {p256dh, auth}}"})
		return
	}
	if err := validPushEndpoint(b.Endpoint, ch.AllowedHosts); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !validKey(b.Keys.P256dh, 65, 0x04) || !validKey(b.Keys.Auth, 16, 0) {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "keys: p256dh (65 bytes) and auth (16 bytes), base64url"})
		return
	}
	err := n.store.AddSubscription(r.Context(), pushSub{Subject: id.Subject, Endpoint: b.Endpoint, P256dh: b.Keys.P256dh,
		Auth: b.Keys.Auth, Created: now()}, maxSubsPerUser)
	switch {
	case errors.Is(err, errTooMany):
		writeJSON(rw, http.StatusConflict, map[string]string{"error": "you have the maximum number of devices; remove one first"})
	case err != nil:
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot store the subscription"})
	default:
		audit(auditEntry{Who: n.w.who(id), Action: "push-subscribe", Result: "ok", Request: map[string]any{"host": hostOf(b.Endpoint)}})
		writeJSON(rw, http.StatusCreated, map[string]string{"state": "subscribed"})
	}
}

// DELETE /api/push/subscriptions?endpoint=...: removes one subscription of the caller (never of another user).
func (n *notifyAPI) unsubscribe(rw http.ResponseWriter, r *http.Request) {
	id, ok := n.touch(rw, r, true)
	if !ok {
		return
	}
	ep := r.URL.Query().Get("endpoint")
	if ep == "" || len(ep) > maxEndpointLen {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "endpoint required"})
		return
	}
	if _, err := n.store.DeleteSubscription(r.Context(), id.Subject, ep); err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot remove the subscription"})
		return
	}
	audit(auditEntry{Who: n.w.who(id), Action: "push-unsubscribe", Result: "ok", Request: map[string]any{"host": hostOf(ep)}})
	writeJSON(rw, http.StatusOK, map[string]string{"state": "removed"})
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// settingsView is GET /api/notify/settings.
type settingsView struct {
	Channels      []string        `json:"channels"`      // on for this instance (NotificationChannel objects)
	Types         map[string]bool `json:"types"`         // every type, with the default filled in
	Mutes         []notifyMute    `json:"mutes"`         // only projects the caller may still see
	Projects      []string        `json:"projects"`      // projects the caller may mute
	Subscriptions []string        `json:"subscriptions"` // endpoint of each device (the UI matches its own)
	Snooze        int             `json:"snoozeMinutes"` // what Snooze in a notification does
	PublicKey     string          `json:"vapidPublicKey,omitempty"`
}

// visibleProjects: the repositories of the agents the caller may see (R35: admin group, or forge write access).
func (n *notifyAPI) visibleProjects(ctx context.Context, id *identity) []string {
	lister, ok := n.w.store.(agentLister)
	if !ok {
		return []string{}
	}
	list, err := lister.List(ctx)
	if err != nil {
		return []string{}
	}
	may := n.w.writeAccess(ctx, id)
	admin := n.w.isAdmin(id)
	seen := map[string]bool{}
	out := []string{}
	for _, a := range list {
		for _, repo := range a.Repos {
			if !seen[repo] && (admin || may(a.Forge, repo)) {
				seen[repo] = true
				out = append(out, repo)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (n *notifyAPI) getSettings(rw http.ResponseWriter, r *http.Request) {
	id, ok := n.touch(rw, r, false)
	if !ok {
		return
	}
	s, err := n.store.Settings(r.Context(), id.Subject)
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read the settings"})
		return
	}
	v := settingsView{Channels: []string{}, Types: map[string]bool{}, Mutes: []notifyMute{}, Subscriptions: []string{},
		Projects: n.visibleProjects(r.Context(), id), Snooze: int(snoozeFor / time.Minute)}
	if ch := n.channels(); ch != nil {
		v.Channels, v.PublicKey = append(v.Channels, "webpush"), ch.PublicKey
	}
	for _, t := range notifyTypes {
		v.Types[t] = s.wants(t, "")
	}
	for _, m := range s.Mutes {
		if slices.Contains(v.Projects, m.Project) {
			v.Mutes = append(v.Mutes, m)
		}
	}
	if subs, err := n.store.Subscriptions(r.Context(), id.Subject); err == nil {
		for _, p := range subs {
			v.Subscriptions = append(v.Subscriptions, p.Endpoint)
		}
	}
	writeJSON(rw, http.StatusOK, v)
}

// PUT /api/notify/settings: {types: {approvals: bool, ...}, mutes: [{project, type}]}. Replaces the caller's settings.
// A mute is accepted only for a project the caller may see; a type is one of notifyTypes ("*" in a mute only).
func (n *notifyAPI) putSettings(rw http.ResponseWriter, r *http.Request) {
	id, ok := n.touch(rw, r, true)
	if !ok {
		return
	}
	var b notifySettings
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyNotify))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "body: {types: {...}, mutes: [{project, type}]}"})
		return
	}
	for t := range b.Types {
		if !slices.Contains(notifyTypes, t) {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "types: approvals, stopped or usage"})
			return
		}
	}
	if len(b.Mutes) > maxMutesPerUser {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "too many mutes"})
		return
	}
	projects := n.visibleProjects(r.Context(), id)
	seen := map[notifyMute]bool{}
	mutes := []notifyMute{}
	for _, m := range b.Mutes {
		if !validRepo(m.Project) || (m.Type != typeAll && !slices.Contains(notifyTypes, m.Type)) {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "mutes: project owner/name, type approvals, stopped, usage or *"})
			return
		}
		if !slices.Contains(projects, m.Project) {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "you cannot see project " + m.Project})
			return
		}
		if !seen[m] {
			seen[m] = true
			mutes = append(mutes, m)
		}
	}
	b.Mutes = mutes
	if b.Types == nil {
		b.Types = map[string]bool{}
	}
	if err := n.store.SaveSettings(r.Context(), id.Subject, b); err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot store the settings"})
		return
	}
	n.getSettings(rw, r)
}

// POST /api/push/action: a Deny or Snooze button of a notification (push.go). No OIDC token: the service worker has
// none. The signed action token is the credential: bound to user, approval, content hash and action, short-lived and
// single-use. Approve is never an action (docs/console-ux.md §4): it opens the drawer.
func (n *notifyAPI) action(rw http.ResponseWriter, r *http.Request) {
	if n.push == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "web push is not enabled on this instance"})
		return
	}
	// one bucket for all callers (#404 review): the API sits behind the UI proxy, so the peer address is the proxy,
	// and X-Forwarded-For is client-supplied. Tokens are HMAC-signed, so this only bounds load.
	if !actionLimit.allow("push-action") {
		writeJSON(rw, http.StatusTooManyRequests, map[string]string{"error": "too many actions; try again in a minute"})
		return
	}
	var b struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&b); err != nil || b.Token == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "body: {token}"})
		return
	}
	code, out := n.push.runAction(r.Context(), b.Token)
	writeJSON(rw, code, out)
}

// actionLimit: POST /api/push/action has no bearer token; it is limited for the whole instance (300 a minute).
var actionLimit = newRateLimiter(300, time.Minute)
