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
	"sort"
	"strings"
	"sync"
	"time"

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
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update

// SecurityConfig holds the knobs (env on the operator, see main.go).
type SecurityConfig struct {
	PauseAfter  int           // more than this many events in one job pause the agent (default 5)
	Retention   time.Duration // SecurityEvent objects older than this are deleted (default 30 days)
	MaxPerAgent int           // newest objects kept per agent (default 500)
	Rescan      time.Duration // how often new pods and containers are picked up
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

	mu      sync.Mutex
	streams map[string]bool // podUID/container being followed
}

// rawEvent is the log line's JSON. Agent and job from the line are informational: the agent is taken from the pod.
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
	h := sha256.Sum256([]byte(string(pod.UID) + "\x00" + container + "\x00" + ts + "\x00" + rest))
	return v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: clip(e.Source, 40), Rule: clip(e.Rule, 80),
		Severity: e.Severity, Agent: agentOf(pod), Job: e.Job, Action: clip(e.Action, 503),
		Time: metav1.NewTime(t), Pod: pod.Name, Container: container, Hash: hex.EncodeToString(h[:])}, true
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
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

func (c *Collector) consume(ctx context.Context, r io.Reader, p *corev1.Pod, container string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		spec, ok := parseLine(sc.Text(), p, container)
		if !ok {
			continue
		}
		if err := c.Record(ctx, p, spec); err != nil {
			log.FromContext(ctx).Error(err, "record security event", "rule", spec.Rule)
			continue // position not advanced: the line is read again after the next reconnect
		}
		if err := c.savePosition(ctx, p, container, spec.Time.Time); err != nil {
			log.FromContext(ctx).Error(err, "save log position")
		}
	}
	return sc.Err()
}

// Record creates the SecurityEvent (idempotent by name) and, for a new one, applies the auto-pause rule.
func (c *Collector) Record(ctx context.Context, p *corev1.Pod, spec v1.SecurityEventSpec) error {
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: c.NS,
		Labels: map[string]string{"agent": spec.Agent, "agentdev/severity": spec.Severity,
			"agentdev/pod-uid": string(p.UID)}}, Spec: spec}
	if err := c.Create(ctx, ev); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // seen before (restart or reconnect)
		}
		return err
	}
	log.FromContext(ctx).Info("security event", "agent", spec.Agent, "rule", spec.Rule, "severity", spec.Severity, "job", spec.Job)
	return c.maybePause(ctx, spec)
}

// maybePause pauses the agent on a high-severity event, or when its job has more than PauseAfter events. A person
// resumes it (spec.paused=false) after looking; the reason stays in the SecurityPaused condition.
func (c *Collector) maybePause(ctx context.Context, spec v1.SecurityEventSpec) error {
	reason, msg := "", ""
	if spec.Severity == "high" {
		reason, msg = "HighSeverity", fmt.Sprintf("high severity %s/%s: %s", spec.Source, spec.Rule, clip(spec.Action, 200))
	} else if spec.Job != "" {
		var list v1.SecurityEventList
		if err := c.List(ctx, &list, client.InNamespace(c.NS), client.MatchingLabels{"agent": spec.Agent}); err != nil {
			return err
		}
		n := 0
		for _, e := range list.Items {
			if e.Spec.Job == spec.Job {
				n++
			}
		}
		if n > c.C.PauseAfter {
			reason, msg = "TooManyEvents", fmt.Sprintf("%d security events in job %s (limit %d)", n, spec.Job, c.C.PauseAfter)
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

// prunePositions drops positions of pods that are gone (a new pod has a new UID and starts from its first line).
func (c *Collector) prunePositions(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cm corev1.ConfigMap
	if err := c.Direct.Get(ctx, types.NamespacedName{Namespace: c.NS, Name: positionsCM}, &cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(c.NS), client.MatchingLabels{"app": "coding-agent"}); err != nil {
		return err
	}
	live := map[string]bool{}
	for _, p := range pods.Items {
		live[string(p.UID)] = true
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
