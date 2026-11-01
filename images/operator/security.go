package main

// Security events (#70): the agent's containers print `AGENTDEV_SECURITY_EVENT {json}` lines on stdout (images/forge
// events.go, images/worker/agent-loop.sh). The collector follows the logs of every agent pod and container, turns each
// line into a SecurityEvent object, pauses the agent when a guardrail fires too often or with high severity, and
// prunes old objects. The log line stays the source of truth (cluster log pipeline); the objects are a working copy.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const eventPrefix = "AGENTDEV_SECURITY_EVENT "

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=agentdev.dev,resources=securityevents,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agentdev.dev,resources=agents,verbs=patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,resourceNames=agentdev-security-positions

// SecurityConfig holds the knobs (env on the operator, see main.go).
type SecurityConfig struct {
	PauseAfter  int           // more than this many events in one job pause the agent (default 5)
	Retention   time.Duration // SecurityEvent objects older than this are deleted (default 30 days)
	MaxPerAgent int           // newest objects kept per agent (default 500)
	Rescan      time.Duration // how often new pods and containers are picked up
	RatePerMin  int           // events per minute one pod container may record (token bucket refill, default 10)
	RateBurst   int           // bucket size (default 20)
}

// logOpener opens a container's log, with runtime timestamps, from since (nil: from the start), following it.
type logOpener func(ctx context.Context, ns, pod, container string, since *metav1.Time) (io.ReadCloser, error)

func kubeLogs(cs kubernetes.Interface) logOpener {
	return func(ctx context.Context, ns, pod, container string, since *metav1.Time) (io.ReadCloser, error) {
		return cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true,
			Timestamps: true, SinceTime: since}).Stream(ctx)
	}
}

type Collector struct {
	client.Client
	NS   string
	Logs logOpener
	C    SecurityConfig
	Now  func() time.Time
	// Direct reads the positions ConfigMap without the cache (read-modify-write); the manager's API reader.
	Direct client.Reader
	// Usage receives the AGENTDEV_USAGE lines (#93); nil: ignored.
	Usage *Usage

	mu      sync.Mutex
	forged  map[string]bool // pod UID/container with a usage line outside the forge container already reported
	streams map[string]bool // podUID/container being followed

	bmu     sync.Mutex
	buckets map[string]*bucket // podUID/container -> token bucket (flood protection)
	Dropped atomic.Int64       // event lines dropped by the rate limit (all pods)
}

// bucket is a token bucket; limited is set once the rate-limited event for the current flood has been recorded.
type bucket struct {
	tokens  float64
	last    time.Time
	limited bool
	// verdicts of recorded lines by hash: a line read again after a failed Record (the stream reconnects from the
	// last saved position) gets its first verdict back and costs no second token. Trimmed to the last 10 minutes.
	verdicts map[string]verdict
}

type verdict struct {
	ok, first bool
	t         time.Time
}

// admit is allow for one parsed line: a line already admitted (same hash) returns its earlier verdict without taking
// a token, so a DB or API outage, where the same lines are re-read on every reconnect, cannot drain the bucket.
func (c *Collector) admit(key, hash string, now time.Time) (ok, first bool) {
	c.bmu.Lock()
	if b := c.buckets[key]; b != nil {
		if v, seen := b.verdicts[hash]; seen {
			c.bmu.Unlock()
			return v.ok, v.first
		}
	}
	c.bmu.Unlock()
	ok, first = c.allow(key, now)
	if ok || first { // only recorded lines can fail and be re-read
		c.bmu.Lock()
		b := c.buckets[key]
		if b.verdicts == nil {
			b.verdicts = map[string]verdict{}
		}
		for h, v := range b.verdicts {
			if now.Sub(v.t) > 10*time.Minute {
				delete(b.verdicts, h)
			}
		}
		b.verdicts[hash] = verdict{ok, first, now}
		c.bmu.Unlock()
	}
	return ok, first
}

// pruneBuckets drops the rate-limit state of pods that are gone (live holds the UIDs of existing pods).
func (c *Collector) pruneBuckets(live map[string]bool) {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	for k := range c.buckets {
		if uid, _, _ := strings.Cut(k, "/"); !live[uid] {
			delete(c.buckets, k)
		}
	}
}

// allow takes one token from the pod container's bucket. first is true for the first refused line of a flood: the
// caller records one aggregated rate-limited event for it, and drops the rest until the bucket refills.
func (c *Collector) allow(key string, now time.Time) (ok, first bool) {
	rate, burst := c.C.RatePerMin, c.C.RateBurst
	if rate <= 0 {
		rate = 10
	}
	if burst <= 0 {
		burst = 20
	}
	c.bmu.Lock()
	defer c.bmu.Unlock()
	if c.buckets == nil {
		c.buckets = map[string]*bucket{}
	}
	b := c.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: now}
		c.buckets[key] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens += d.Minutes() * float64(rate)
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		b.limited = false
		return true, false
	}
	first = !b.limited
	b.limited = true
	return false, first
}

// forgeContainer is the forge sidecar (render.go). Only its lines are trusted to name their source: Claude runs in
// the agent container and can print any line there, so those events are always source=loop, untrusted=true.
const forgeContainer = "forge"

var jobRE = regexp.MustCompile(`^(issue|pr)-[0-9]{1,12}$`)

// rawEvent is the log line's JSON. Agent and source from the line are ignored (taken from the pod and container).
type rawEvent struct {
	Schema   string `json:"schema"`
	Source   string `json:"source"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Agent    string `json:"agent"`
	Job      string `json:"job"`
	Action   string `json:"action"`
}

// parseLine turns one timestamped log line ("<RFC3339Nano> AGENTDEV_SECURITY_EVENT {...}") into an event spec.
// ok=false for any other line. The hash covers pod UID, container, timestamp and the raw line, so the same line read
// twice (operator restart, stream reconnect) gives the same object name, while two identical refusals do not.
func parseLine(line string, pod *corev1.Pod, container string) (v1.SecurityEventSpec, bool) {
	ts, rest, found := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	if !found || !strings.HasPrefix(rest, eventPrefix) {
		return v1.SecurityEventSpec{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return v1.SecurityEventSpec{}, false
	}
	var e rawEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rest, eventPrefix)), &e); err != nil || e.Rule == "" {
		return v1.SecurityEventSpec{}, false
	}
	switch e.Severity {
	case "low", "medium", "high":
	default:
		e.Severity = "high" // an unknown severity is treated as the worst case, never dropped
	}
	source, untrusted := "forge", false
	if container != forgeContainer {
		source, untrusted = "loop", true
	}
	if !jobRE.MatchString(e.Job) {
		e.Job = "" // not a job name: a forged or garbled value is not stored
	}
	h := sha256.Sum256([]byte(string(pod.UID) + "\x00" + container + "\x00" + ts + "\x00" + rest))
	return v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: source, Untrusted: untrusted, Rule: clip(e.Rule, 80),
		Severity: e.Severity, Agent: agentOf(pod), Job: e.Job, Action: clip(e.Action, 503),
		Time: metav1.NewTime(t), Pod: pod.Name, Container: container, Hash: hex.EncodeToString(h[:])}, true
}

// clip cuts s to at most n bytes on a UTF-8 rune boundary (Postgres rejects invalid UTF-8); invalid input bytes are
// replaced first.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func agentOf(p *corev1.Pod) string {
	if a := p.Labels["agent"]; a != "" {
		return a
	}
	return strings.TrimSuffix(strings.TrimPrefix(p.Name, "agent-"), "-0")
}

func eventName(s v1.SecurityEventSpec) string { return s.Agent + "-" + s.Hash[:16] }

// Start implements manager.Runnable: rescans agent pods and follows each container until ctx ends.
func (c *Collector) Start(ctx context.Context) error {
	c.streams = map[string]bool{}
	tick := time.NewTicker(c.C.Rescan)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		c.scan(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-prune.C:
			if err := c.Prune(ctx); err != nil {
				log.FromContext(ctx).Error(err, "prune security events")
			}
			if n := c.Dropped.Load(); n > 0 {
				log.FromContext(ctx).Info("security event lines dropped by the rate limit (total since start)", "dropped", n)
			}
		case <-tick.C:
		}
	}
}

func (c *Collector) scan(ctx context.Context) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(c.NS), client.MatchingLabels{"app": "coding-agent"}); err != nil {
		log.FromContext(ctx).Error(err, "list agent pods")
		return
	}
	for i := range pods.Items {
		p := pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, ct := range p.Spec.Containers {
			key := string(p.UID) + "/" + ct.Name
			c.mu.Lock()
			busy := c.streams[key]
			c.streams[key] = true
			c.mu.Unlock()
			if !busy {
				go c.follow(ctx, &p, ct.Name, key)
			}
		}
	}
}

// follow reads one container's log from the last recorded event of that container (position kept in the cluster,
// so an operator restart resumes there; the hash drops the boundary duplicates). Ends when the stream ends; the next
// scan starts it again if the pod still runs.
func (c *Collector) follow(ctx context.Context, p *corev1.Pod, container, key string) {
	defer func() { c.mu.Lock(); delete(c.streams, key); c.mu.Unlock() }()
	l := log.FromContext(ctx).WithValues("pod", p.Name, "container", container)
	since, err := c.position(ctx, p, container)
	if err != nil {
		l.Error(err, "read position")
		return
	}
	rc, err := c.Logs(ctx, c.NS, p.Name, container, since)
	if err != nil {
		l.V(1).Info("open log", "err", err.Error())
		return
	}
	defer rc.Close()
	if err := c.consume(ctx, rc, p, container); err != nil && ctx.Err() == nil {
		l.Error(err, "read log")
	}
}

// positionsCM keeps, per pod UID and container, the timestamp of the last event line read. It lives in the cluster
// (not in the operator's memory), so a restarted operator resumes there and pruned events are not re-created.
const positionsCM = "agentdev-security-positions"

func posKey(p *corev1.Pod, container string) string { return string(p.UID) + "." + container }

// position is the last recorded line time of this pod and container, or nil (read from the start).
func (c *Collector) position(ctx context.Context, p *corev1.Pod, container string) (*metav1.Time, error) {
	var cm corev1.ConfigMap
	if err := c.Direct.Get(ctx, types.NamespacedName{Namespace: c.NS, Name: positionsCM}, &cm); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	v, ok := cm.Data[posKey(p, container)]
	if !ok {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, nil
	}
	mt := metav1.NewTime(t)
	return &mt, nil
}

// savePosition stores the time of the line just recorded; entries of pods that no longer exist are dropped by Prune.
func (c *Collector) savePosition(ctx context.Context, p *corev1.Pod, container string, t time.Time) error {
	c.mu.Lock() // one writer at a time for the shared ConfigMap
	defer c.mu.Unlock()
	var cm corev1.ConfigMap
	err := c.Direct.Get(ctx, types.NamespacedName{Namespace: c.NS, Name: positionsCM}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: positionsCM, Namespace: c.NS},
			Data: map[string]string{posKey(p, container): t.UTC().Format(time.RFC3339Nano)}}
		return c.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[posKey(p, container)] = t.UTC().Format(time.RFC3339Nano)
	return c.Update(ctx, &cm)
}

// consume records the event lines of one stream. On a Record failure it stops without advancing the position, so the
// next scan reconnects from the last recorded line and the failed one is read again. Lines over the rate limit are
// dropped (counted in Dropped); the first of a flood becomes one rate-limited high event, which pauses the agent.
func (c *Collector) consume(ctx context.Context, r io.Reader, p *corev1.Pod, container string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// usage lines (#93): accepted only from the forge sidecar, which prints them from the loop's structured
		// POST /usage report after checking it (review #102). The agent container's log carries Claude's free
		// text, so a usage line there is never taken; it is dropped and reported once per pod.
		if c.Usage != nil && p.Labels["agent"] != "" && isUsageLine(sc.Text()) {
			if container != forgeContainer {
				c.forgedUsage(ctx, p, container, sc.Text())
				continue
			}
			if u, at, ok := parseUsage(sc.Text()); ok {
				if err := c.Usage.Observe(ctx, p, u, at); err != nil {
					log.FromContext(ctx).Error(err, "record usage")
				}
			}
			continue
		}
		spec, ok := parseLine(sc.Text(), p, container)
		if !ok {
			continue
		}
		if ok, first := c.admit(string(p.UID)+"/"+container, spec.Hash, spec.Time.Time); !ok {
			c.Dropped.Add(1)
			if !first {
				continue
			}
			spec = rateLimited(spec, c.C.RatePerMin)
		}
		if err := c.Record(ctx, p, spec); err != nil {
			return fmt.Errorf("record security event %s: %w", spec.Rule, err)
		}
		if err := c.savePosition(ctx, p, container, spec.Time.Time); err != nil {
			log.FromContext(ctx).Error(err, "save log position")
		}
	}
	return sc.Err()
}

// rateLimited turns the first refused line of a flood into the aggregated event (same time, pod and container).
func rateLimited(s v1.SecurityEventSpec, rate int) v1.SecurityEventSpec {
	h := sha256.Sum256([]byte("rate-limited\x00" + s.Hash))
	s.Rule, s.Severity, s.Job = "rate-limited", "high", ""
	s.Action = fmt.Sprintf("more than %d security events per minute from container %s; further lines dropped", rate, s.Container)
	s.Hash = hex.EncodeToString(h[:])
	return s
}

// Record creates the SecurityEvent (idempotent by name) and, for a new one, applies the auto-pause rule.
func (c *Collector) Record(ctx context.Context, p *corev1.Pod, spec v1.SecurityEventSpec) error {
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: c.NS,
		Labels: map[string]string{"agent": spec.Agent, "agentdev/severity": spec.Severity,
			"agentdev/pod-uid": string(p.UID), "agentdev/job": spec.Job}}, Spec: spec}
	if err := c.Create(ctx, ev); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // seen before (restart or reconnect)
		}
		return err
	}
	log.FromContext(ctx).Info("security event", "agent", spec.Agent, "rule", spec.Rule, "severity", spec.Severity, "job", spec.Job)
	return c.maybePause(ctx, p, ev.Name, spec)
}

// maybePause pauses the agent on a high-severity event, or when the current run has more than PauseAfter events.
// The run is the pod UID plus job: a new pod starts a new count, and events without a job share one per-pod bucket.
// The count includes the event just created (name), even if the cache has not seen it yet. A person resumes the agent
// (spec.paused=false) after looking; the reason stays in the SecurityPaused condition.
func (c *Collector) maybePause(ctx context.Context, p *corev1.Pod, name string, spec v1.SecurityEventSpec) error {
	reason, msg := "", ""
	if spec.Severity == "high" {
		reason, msg = "HighSeverity", fmt.Sprintf("high severity %s/%s: %s", spec.Source, spec.Rule, clip(spec.Action, 200))
	} else {
		var list v1.SecurityEventList
		if err := c.List(ctx, &list, client.InNamespace(c.NS), client.MatchingLabels{"agentdev/pod-uid": string(p.UID)}); err != nil {
			return err
		}
		n := 1 // the event just recorded
		for _, e := range list.Items {
			if e.Name != name && e.Spec.Job == spec.Job {
				n++
			}
		}
		if n > c.C.PauseAfter {
			job := spec.Job
			if job == "" {
				job = "(none)"
			}
			reason, msg = "TooManyEvents", fmt.Sprintf("%d security events in job %s of pod %s (limit %d)", n, job, p.Name, c.C.PauseAfter)
		}
	}
	if reason == "" {
		return nil
	}
	key := types.NamespacedName{Namespace: c.NS, Name: spec.Agent}
	var a v1.Agent
	if err := c.Direct.Get(ctx, key, &a); err != nil {
		return client.IgnoreNotFound(err) // an agent from before the operator: nothing to pause here
	}
	if !a.Spec.Paused {
		patch := client.MergeFrom(a.DeepCopy())
		a.Spec.Paused = true
		if err := c.Patch(ctx, &a, patch); err != nil {
			return fmt.Errorf("pause %s: %w", a.Name, err)
		}
		log.FromContext(ctx).Info("agent paused by security event", "agent", a.Name, "reason", reason)
	}
	// the reconciler writes status too: re-read and retry on a conflict so the reason is not lost
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Direct.Get(ctx, key, &a); err != nil {
			return err
		}
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: "SecurityPaused", Status: metav1.ConditionTrue,
			Reason: reason, Message: msg, ObservedGeneration: a.Generation})
		return c.Status().Update(ctx, &a)
	})
}

// Prune deletes SecurityEvents older than Retention and, per agent, all but the newest MaxPerAgent.
func (c *Collector) Prune(ctx context.Context) error {
	var list v1.SecurityEventList
	if err := c.List(ctx, &list, client.InNamespace(c.NS)); err != nil {
		return err
	}
	cut := c.Now().Add(-c.C.Retention)
	byAgent := map[string][]*v1.SecurityEvent{}
	var drop []*v1.SecurityEvent
	for i := range list.Items {
		e := &list.Items[i]
		if e.Spec.Time.Time.Before(cut) {
			drop = append(drop, e)
			continue
		}
		byAgent[e.Spec.Agent] = append(byAgent[e.Spec.Agent], e)
	}
	for _, es := range byAgent {
		if len(es) <= c.C.MaxPerAgent {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Spec.Time.After(es[j].Spec.Time.Time) })
		drop = append(drop, es[c.C.MaxPerAgent:]...)
	}
	for _, e := range drop {
		if err := c.Delete(ctx, e); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return c.prunePositions(ctx)
}

// prunePositions drops positions and rate-limit buckets of pods that are gone (a new pod has a new UID and starts from its first line).
func (c *Collector) prunePositions(ctx context.Context) error {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(c.NS), client.MatchingLabels{"app": "coding-agent"}); err != nil {
		return err
	}
	live := map[string]bool{}
	for _, p := range pods.Items {
		live[string(p.UID)] = true
	}
	c.pruneBuckets(live)
	c.mu.Lock()
	defer c.mu.Unlock()
	var cm corev1.ConfigMap
	if err := c.Direct.Get(ctx, types.NamespacedName{Namespace: c.NS, Name: positionsCM}, &cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	changed := false
	for k := range cm.Data {
		if uid, _, _ := strings.Cut(k, "."); !live[uid] {
			delete(cm.Data, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.Update(ctx, &cm)
}

// forgedUsage reports a usage line printed outside the structured channel (the agent container): one medium event per
// pod and container, so a model that prints many does not flood the event store.
func (c *Collector) forgedUsage(ctx context.Context, p *corev1.Pod, container, line string) {
	c.mu.Lock()
	if c.forged == nil {
		c.forged = map[string]bool{}
	}
	key := string(p.UID) + "/" + container
	seen := c.forged[key]
	c.forged[key] = true
	c.mu.Unlock()
	if seen {
		return
	}
	ts, _, _ := strings.Cut(line, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t = c.Now()
	}
	h := sha256.Sum256([]byte(string(p.UID) + "\x00" + container + "\x00usage-forged"))
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "operator", Rule: "usage-line-forged", Severity: "medium",
		Agent: p.Labels["agent"], Action: "AGENTDEV_USAGE line in the " + container + " container log ignored: usage is accepted only from the forge sidecar",
		Time: metav1.NewTime(t), Pod: p.Name, Container: container, Hash: hex.EncodeToString(h[:])}
	if err := c.Record(ctx, p, spec); err != nil {
		log.FromContext(ctx).Error(err, "record forged usage event")
	}
}
