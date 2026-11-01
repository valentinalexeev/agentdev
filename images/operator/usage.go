package main

// Subscription usage limits (#93, incident #92). After each Claude Code run the worker prints one
// `AGENTDEV_USAGE {json}` line: tokens, and the 5-hour and weekly rate-limit windows the CLI reported. The collector
// (security.go) hands these lines to Usage. Usage keeps one state per Claude credential Secret (a subscription; agents
// that share claude-shared share one budget), and pauses new runs of the agents on that subscription when a window
// passes its threshold, or at once when a run hit the rate limit. Runs in progress finish. The pause is a time
// (pausedUntil = the reset of the window) in the ConfigMap agentdev-usage, which every agent pod mounts read-only;
// the worker starts no new job before that time. So the resume is automatic, even when the operator is down.
//
// Thresholds: the ConfigMap agentdev-usage-settings (written by the API: admin sets the global value, a user with
// write access to a project a lower one for that project). The effective threshold of an agent is the lower of the
// global and its project's value: a project value above the global one never raises the limit (clamp).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,resourceNames=agentdev-usage
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get,resourceNames=agentdev-usage-settings

const (
	usagePrefix     = "AGENTDEV_USAGE "
	usageCM         = "agentdev-usage"          // state and pauses, written by the operator, mounted into agent pods
	usageSettingsCM = "agentdev-usage-settings" // thresholds, written by the API
	// rateLimitPause is used when a run hit the rate limit and the CLI gave no reset time.
	rateLimitPause = time.Hour
	// maxReportPause caps how long one report can pause (review #102): a pause from a window or a rate limit ends at
	// most this long after the report that caused it. If the limit is real, the next run reports it again.
	maxReportPause = 5 * time.Hour
	// anomalyJump: a rise of utilization by more than this in one report of the same window raises a security event.
	anomalyJump = 0.5
)

// DefaultFiveHour and DefaultWeekly are the global thresholds in percent when no setting exists (#92 decision: 80 and
// 90). The instance can change them with USAGE_FIVE_HOUR and USAGE_WEEKLY (config.env, Ansible usage.*), set in main.
var (
	DefaultFiveHour = 80
	DefaultWeekly   = 90
)

// Thresholds in percent of a window. 0 means "not set" in a project override.
type Thresholds struct {
	FiveHour int `json:"fiveHour,omitempty"`
	Weekly   int `json:"weekly,omitempty"`
}

// UsageSettings is settings.json in agentdev-usage-settings. Projects are keyed by owner/name.
type UsageSettings struct {
	Global   Thresholds            `json:"global"`
	Projects map[string]Thresholds `json:"projects,omitempty"`
}

// Effective returns the thresholds for one project: the global values (defaults when unset), lowered by the
// project's values. A project value above the global one is ignored (clamped), so it can never raise the limit.
func (s UsageSettings) Effective(repo string) Thresholds {
	t := Thresholds{FiveHour: DefaultFiveHour, Weekly: DefaultWeekly}
	if valid(s.Global.FiveHour) {
		t.FiveHour = s.Global.FiveHour
	}
	if valid(s.Global.Weekly) {
		t.Weekly = s.Global.Weekly
	}
	if p, ok := s.Projects[repo]; ok {
		if valid(p.FiveHour) && p.FiveHour < t.FiveHour {
			t.FiveHour = p.FiveHour
		}
		if valid(p.Weekly) && p.Weekly < t.Weekly {
			t.Weekly = p.Weekly
		}
	}
	return t
}

// EffectiveAll is the lowest effective threshold over all repositories of an agent. An Agent has one (spec.repo)
// today; the API uses the same rule (images/api/usage.go), so a limit shown as effective always applies.
func (s UsageSettings) EffectiveAll(repos ...string) Thresholds {
	t := s.Effective("")
	for _, r := range repos {
		e := s.Effective(r)
		t.FiveHour, t.Weekly = min(t.FiveHour, e.FiveHour), min(t.Weekly, e.Weekly)
	}
	return t
}

func valid(p int) bool { return p >= 1 && p <= 100 }

// Window is one rate-limit window of a subscription as last reported.
type Window struct {
	Utilization float64 `json:"utilization"` // fraction used, 0..1 (can pass 1)
	ResetsAt    int64   `json:"resetsAt"`    // Unix seconds; 0 = unknown
	// PauseCap: a pause from this window ends at the latest then (the last report that raised it + maxReportPause).
	// Only ever moves later; 0 = no cap known (old state), the reset applies.
	PauseCap int64 `json:"pauseCap,omitempty"`
}

// SubscriptionState is the usage of one Claude credential Secret.
type SubscriptionState struct {
	FiveHour     Window    `json:"fiveHour"`
	Weekly       Window    `json:"weekly"`
	InputTokens  int64     `json:"inputTokens"`  // sum over runs since the last 5-hour reset
	OutputTokens int64     `json:"outputTokens"` // same
	Runs         int64     `json:"runs"`
	RateLimited  int64     `json:"rateLimitedUntil,omitempty"` // a run hit the limit: paused until (Unix seconds)
	Updated      time.Time `json:"updated"`
}

// Pause is why and until when the agents of a subscription start no new run.
type Pause struct {
	Until  int64  `json:"until"`  // Unix seconds
	Reason string `json:"reason"` // five-hour | weekly | rate-limited
}

// usageLine is the worker's AGENTDEV_USAGE JSON. Agent and Secret come from the pod and the Agent, not the line.
type usageLine struct {
	Agent        string  `json:"agent"`  // claude-proxy lines only (#66); ignored from the forge sidecar
	Secret       string  `json:"secret"` // claude-proxy lines only
	Job          string  `json:"job"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	RateLimited  bool    `json:"rate_limited"`
	ResetsAt     int64   `json:"resets_at"`
	FiveHour     *Window `json:"five_hour"`
	Weekly       *Window `json:"seven_day"`
}

// isUsageLine: a timestamped log line that carries a usage report (from any container).
func isUsageLine(line string) bool {
	_, rest, found := strings.Cut(line, " ")
	return found && strings.HasPrefix(rest, usagePrefix)
}

// parseUsage reads one timestamped log line; ok=false for any other line.
func parseUsage(line string) (usageLine, time.Time, bool) {
	ts, rest, found := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	if !found || !strings.HasPrefix(rest, usagePrefix) {
		return usageLine{}, time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return usageLine{}, time.Time{}, false
	}
	var u usageLine
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rest, usagePrefix)), &u); err != nil {
		return usageLine{}, time.Time{}, false
	}
	clean := func(w *Window) *Window {
		if w == nil || math.IsNaN(w.Utilization) || w.Utilization < 0 || w.Utilization > 10 {
			return nil
		}
		return w
	}
	u.FiveHour, u.Weekly = clean(u.FiveHour), clean(u.Weekly)
	if u.InputTokens < 0 {
		u.InputTokens = 0
	}
	if u.OutputTokens < 0 {
		u.OutputTokens = 0
	}
	return u, t, true
}

// Usage holds the subscription states and writes the pauses.
type Usage struct {
	client.Client
	Direct client.Reader
	NS     string
	Now    func() time.Time

	mu     sync.Mutex
	last   map[string]time.Time          // pod UID/container -> time of the last line taken (a log re-read counts once)
	subs   map[string]*SubscriptionState // by Claude Secret name
	loaded bool
	paused map[string]Pause // by agent name, last written
}

// merge takes a new report of a window at time at; length is the window's length. A report can add a pause but
// never lift or shorten one (review #102, owner: "the unsafe usage reset"): within the current window utilization
// only rises and the reset time only moves later; a report starts a new window only after the current one has really
// reset (its ResetsAt is past). A report with an earlier reset than the current one is ignored. Returns whether a new
// window started, and an anomaly text for a security event ("" when none).
func merge(cur *Window, w *Window, at time.Time, length time.Duration) (bool, string) {
	if w == nil {
		return false, ""
	}
	if w.ResetsAt != 0 && w.ResetsAt < cur.ResetsAt {
		return false, "" // an older window reported late
	}
	newWindow := cur.ResetsAt == 0 || cur.ResetsAt <= at.Unix()
	anomaly := ""
	if newWindow {
		started := cur.ResetsAt != 0
		*cur = Window{Utilization: w.Utilization, ResetsAt: w.ResetsAt}
		if w.ResetsAt == 0 {
			cur.ResetsAt = at.Add(length).Unix() // unknown reset: assume a full window, the pause cap still applies
		}
		cur.PauseCap = at.Add(maxReportPause).Unix()
		return started, anomaly
	}
	if w.Utilization-cur.Utilization > anomalyJump {
		anomaly = fmt.Sprintf("utilization jumped from %.2f to %.2f in one report", cur.Utilization, w.Utilization)
	}
	if w.ResetsAt > cur.ResetsAt {
		// same window, later reset: taken (it can only extend a pause, never shorten it), so an early reset that a
		// forged report planted is corrected by the next real one; the per-report cap still bounds the pause
		cur.ResetsAt = w.ResetsAt
	}
	if w.Utilization > cur.Utilization {
		cur.Utilization = w.Utilization
		cur.PauseCap = max(cur.PauseCap, at.Add(maxReportPause).Unix())
	}
	return false, anomaly
}

// capReset cuts a reset time beyond the window length; returns true when it had to (an anomaly).
func capReset(w *Window, at time.Time, d time.Duration) bool {
	if w != nil && w.ResetsAt > at.Add(d).Unix() {
		w.ResetsAt = at.Add(d).Unix()
		return true
	}
	return false
}

// apply adds one observation to the state of a subscription and returns the anomalies found (for security events).
func (s *SubscriptionState) apply(u usageLine, at time.Time) []string {
	var anomalies []string
	// a reset time can not be later than the window is long
	if capReset(u.FiveHour, at, 5*time.Hour) {
		anomalies = append(anomalies, "five-hour resetsAt beyond the window length")
	}
	if capReset(u.Weekly, at, 7*24*time.Hour) {
		anomalies = append(anomalies, "weekly resetsAt beyond the window length")
	}
	for _, w := range []*Window{u.FiveHour, u.Weekly} {
		if w != nil && w.Utilization > 1.5 {
			anomalies = append(anomalies, fmt.Sprintf("utilization %.2f far above 1", w.Utilization))
		}
	}
	newFive, a := merge(&s.FiveHour, u.FiveHour, at, 5*time.Hour)
	if a != "" {
		anomalies = append(anomalies, "five-hour: "+a)
	}
	if newFive {
		s.InputTokens, s.OutputTokens, s.Runs = 0, 0, 0 // a new 5-hour window
	}
	if _, a := merge(&s.Weekly, u.Weekly, at, 7*24*time.Hour); a != "" {
		anomalies = append(anomalies, "weekly: "+a)
	}
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.Runs++
	if u.RateLimited {
		until := u.ResetsAt
		if until <= at.Unix() {
			until = at.Add(rateLimitPause).Unix()
		}
		if limit := at.Add(maxReportPause).Unix(); until > limit {
			anomalies = append(anomalies, "rate-limit reset beyond the per-report pause cap")
			until = limit
		}
		if until > s.RateLimited {
			s.RateLimited = until // only ever extended
		}
	}
	s.Updated = at.UTC()
	return anomalies
}

// decide returns the pause of an agent on this subscription with thresholds t at now, or ok=false.
// A window counts only until it resets: after ResetsAt its utilization is stale, so the pause ends by itself.
func decide(s *SubscriptionState, t Thresholds, now time.Time) (Pause, bool) {
	n := now.Unix()
	var p Pause
	take := func(until int64, reason string) {
		if until > p.Until {
			p = Pause{Until: until, Reason: reason}
		}
	}
	if s.RateLimited > n {
		take(s.RateLimited, "rate-limited")
	}
	end := func(w Window) int64 { // the reset, cut at the per-report cap
		if w.PauseCap != 0 && w.PauseCap < w.ResetsAt {
			return w.PauseCap
		}
		return w.ResetsAt
	}
	over := func(w Window, pct int) bool {
		return end(w) > n && w.Utilization*100 >= float64(pct)
	}
	if over(s.FiveHour, t.FiveHour) {
		take(end(s.FiveHour), "five-hour")
	}
	if over(s.Weekly, t.Weekly) {
		take(end(s.Weekly), "weekly")
	}
	return p, p.Until > 0
}

// Observe records one usage line of an agent pod and re-evaluates the pauses.
func (u *Usage) Observe(ctx context.Context, p *corev1.Pod, line usageLine, at time.Time) error {
	return u.observe(ctx, p, agentOf(p), forgeContainer, line, at)
}

// ObserveProxy records one usage line of the Claude credential proxy (#66). The proxy took the agent from the agent's
// verified key, so the agent and the subscription come from the line; the model cannot write to this log.
func (u *Usage) ObserveProxy(ctx context.Context, p *corev1.Pod, line usageLine, at time.Time) error {
	if !agentNameRE.MatchString(line.Agent) {
		return nil
	}
	return u.observe(ctx, p, line.Agent, proxyContainer, line, at)
}

func (u *Usage) observe(ctx context.Context, p *corev1.Pod, agent, container string, line usageLine, at time.Time) error {
	var a v1.Agent
	if err := u.Direct.Get(ctx, types.NamespacedName{Namespace: u.NS, Name: agent}, &a); err != nil {
		return client.IgnoreNotFound(err) // an agent without an Agent object: no subscription known
	}
	u.mu.Lock()
	if err := u.loadLocked(ctx); err != nil {
		u.mu.Unlock()
		return err
	}
	key := string(p.UID) + "/" + container + "/" + agent
	if u.last == nil {
		u.last = map[string]time.Time{}
	}
	if !at.After(u.last[key]) {
		u.mu.Unlock()
		return nil // read again after a stream reconnect
	}
	u.last[key] = at
	// the subscription: the Agent's Claude Secret, or the one the proxy really used (claude-<agent> or claude-shared)
	secret := a.Spec.ClaudeSecret
	if container == proxyContainer && (line.Secret == "claude-shared" || line.Secret == "claude-"+agent) {
		secret = line.Secret
	}
	s := u.subs[secret]
	if s == nil {
		s = &SubscriptionState{}
		u.subs[secret] = s
	}
	anomalies := s.apply(line, at)
	u.mu.Unlock()
	for _, an := range anomalies {
		u.anomaly(ctx, a.Name, secret, p, container, at, an)
	}
	log.FromContext(ctx).V(1).Info("usage", "agent", a.Name, "secret", secret, "rateLimited", line.RateLimited, "source", container)
	return u.Evaluate(ctx)
}

// loadLocked reads the saved states once (operator restart).
func (u *Usage) loadLocked(ctx context.Context) error {
	if u.loaded {
		return nil
	}
	u.subs, u.paused = map[string]*SubscriptionState{}, map[string]Pause{}
	var cm corev1.ConfigMap
	err := u.Direct.Get(ctx, types.NamespacedName{Namespace: u.NS, Name: usageCM}, &cm)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		_ = json.Unmarshal([]byte(cm.Data["state.json"]), &u.subs)
		_ = json.Unmarshal([]byte(cm.Data["pauses.json"]), &u.paused)
		if u.subs == nil {
			u.subs = map[string]*SubscriptionState{}
		}
		if u.paused == nil {
			u.paused = map[string]Pause{}
		}
	}
	u.loaded = true
	return nil
}

func (u *Usage) settings(ctx context.Context) UsageSettings {
	var s UsageSettings
	var cm corev1.ConfigMap
	if err := u.Direct.Get(ctx, types.NamespacedName{Namespace: u.NS, Name: usageSettingsCM}, &cm); err == nil {
		if err := json.Unmarshal([]byte(cm.Data["settings.json"]), &s); err != nil {
			log.FromContext(ctx).Error(err, "agentdev-usage-settings: settings.json is not valid; defaults apply")
			s = UsageSettings{}
		}
	}
	return s
}

// Evaluate computes the pause of every agent, writes agentdev-usage and records a security event for each new pause.
// Called after each observation and periodically (a changed threshold, a reset window).
func (u *Usage) Evaluate(ctx context.Context) error {
	var agents v1.AgentList
	if err := u.List(ctx, &agents, client.InNamespace(u.NS)); err != nil {
		return err
	}
	settings := u.settings(ctx)
	now := u.Now()

	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.loadLocked(ctx); err != nil {
		return err
	}
	next := map[string]Pause{}
	for _, a := range agents.Items {
		s := u.subs[a.Spec.ClaudeSecret]
		if s == nil {
			continue
		}
		if p, ok := decide(s, settings.EffectiveAll(a.Spec.Repo), now); ok {
			next[a.Name] = p
		}
	}
	var started []string
	for name, p := range next {
		if old, ok := u.paused[name]; !ok || old.Until <= now.Unix() || old.Reason != p.Reason {
			started = append(started, name)
		}
	}
	sort.Strings(started)
	if err := u.writeLocked(ctx, next); err != nil {
		return err
	}
	for _, name := range started {
		var secret, repo string
		for _, a := range agents.Items {
			if a.Name == name {
				secret, repo = a.Spec.ClaudeSecret, a.Spec.Repo
			}
		}
		p := next[name]
		t := settings.EffectiveAll(repo)
		u.event(ctx, name, p, fmt.Sprintf("new runs paused on subscription %s until %s: %s (thresholds %d%% 5h, %d%% weekly)",
			secret, time.Unix(p.Until, 0).UTC().Format(time.RFC3339), p.Reason, t.FiveHour, t.Weekly), now)
	}
	for name := range u.paused {
		if _, still := next[name]; !still {
			log.FromContext(ctx).Info("usage pause ended", "agent", name)
		}
	}
	u.paused = next
	return nil
}

// writeLocked stores the states and the pauses. One key per paused agent (value: Unix seconds) is what the worker
// reads; the JSON keys are for the operator and the API.
func (u *Usage) writeLocked(ctx context.Context, pauses map[string]Pause) error {
	state, _ := json.Marshal(u.subs)
	pj, _ := json.Marshal(pauses)
	data := map[string]string{"state.json": string(state), "pauses.json": string(pj)}
	for name, p := range pauses {
		data["paused."+name] = strconv.FormatInt(p.Until, 10)
	}
	var cm corev1.ConfigMap
	err := u.Direct.Get(ctx, types.NamespacedName{Namespace: u.NS, Name: usageCM}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageCM, Namespace: u.NS}, Data: data}
		return u.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	if equalData(cm.Data, data) {
		return nil
	}
	cm.Data = data
	return u.Update(ctx, &cm)
}

func equalData(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// event records the pause as a SecurityEvent (source operator, low severity: no auto-pause of the agent itself).
func (u *Usage) event(ctx context.Context, agent string, p Pause, action string, now time.Time) {
	h := sha256.Sum256([]byte(fmt.Sprintf("usage\x00%s\x00%s\x00%d", agent, p.Reason, p.Until)))
	// one event per agent (the agent is the unit the Security page and the SIEM filter by); the action names the
	// subscription, so N agents on one Secret give N events with the same subscription
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "operator", Rule: "usage-" + p.Reason, Severity: "low",
		Agent: agent, Action: clip(action, 500), Time: metav1.NewTime(now), Pod: "", Container: "", Hash: hex.EncodeToString(h[:])}
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: u.NS,
		Labels: map[string]string{"agent": agent, "agentdev/severity": spec.Severity}}, Spec: spec}
	if err := u.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
		log.FromContext(ctx).Error(err, "record usage pause event", "agent", agent)
		return
	}
	log.FromContext(ctx).Info("usage pause", "agent", agent, "reason", p.Reason, "until", p.Until)
}

// anomaly records a suspicious usage report (a jump, a reset beyond the window) as a medium SecurityEvent: the
// report may be forged by the model (R47). The report itself was still applied within the caps.
func (u *Usage) anomaly(ctx context.Context, agent, secret string, p *corev1.Pod, container string, at time.Time, what string) {
	h := sha256.Sum256([]byte(fmt.Sprintf("usage-anomaly\x00%s\x00%s\x00%s\x00%s", p.UID, agent, at.Format(time.RFC3339Nano), what)))
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "operator", Rule: "usage-anomaly", Severity: "medium",
		Agent: agent, Action: clip(fmt.Sprintf("usage report on subscription %s: %s", secret, what), 500),
		Time: metav1.NewTime(at), Pod: p.Name, Container: container, Hash: hex.EncodeToString(h[:])}
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: u.NS,
		Labels: map[string]string{"agent": agent, "agentdev/severity": spec.Severity}}, Spec: spec}
	if err := u.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
		log.FromContext(ctx).Error(err, "record usage anomaly", "agent", agent)
		return
	}
	log.FromContext(ctx).Info("usage anomaly", "agent", agent, "what", what)
}

// Start implements manager.Runnable: re-evaluates every interval (threshold changes, window resets).
func (u *Usage) Start(ctx context.Context) error {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := u.Evaluate(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "evaluate usage")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
