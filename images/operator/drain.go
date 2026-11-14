package main

// Drain before replace (#174). Agent StatefulSets use updateStrategy OnDelete, so a changed pod template (an upgrade:
// new image_tag, new operator) does not kill a running job. The operator replaces each outdated pod itself:
//
//  1. it marks the pod: annotation agentdev/drain=<unix seconds>, which the agent reads through a downward API file
//     (/etc/agentdev/pod/annotations). The agent container has no service account token, so only the operator (or
//     an admin) can set it;
//  2. the loop starts no new job while drained; when its running jobs have ended it asks its forge sidecar
//     (POST /drained), which prints the line AGENTDEV_DRAINED on the forge container's stdout. The collector takes
//     that line only from the forge container (Claude runs in the agent container and can print anything there);
//  3. on that line, or when the pod is not running or not ready, or after DRAIN_MAX_MINUTES, the operator deletes
//     the pod and the StatefulSet controller creates it from the new template. The SIGTERM handler of the loop saves
//     the work in progress of a job that is still running (forced case), and the job continues on the new pod (#175).
//
// A tool Claude runs can reach the sidecar on localhost and send POST /drained early: the result is only that its own
// pod is replaced sooner, the same as the forced case (docs/security.md R105).

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	drainAnnotation = "agentdev/drain"
	drainedPrefix   = "AGENTDEV_DRAINED"
	// UpdatedCondition on the Agent: True when the pod runs the current template (or there is no pod).
	UpdatedCondition = "Updated"
)

// DrainSignals holds the pods whose forge sidecar reported "drained" (by pod UID). Filled by the collector, read by
// the reconciler. In memory: after an operator restart a drained agent reports again on its next loop pass.
type DrainSignals struct {
	mu sync.Mutex
	m  map[types.UID]time.Time
}

func (d *DrainSignals) Mark(uid types.UID, at time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[types.UID]time.Time{}
	}
	d.m[uid] = at
}

func (d *DrainSignals) Drained(uid types.UID) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.m[uid]
	return ok
}

func (d *DrainSignals) Forget(uid types.UID) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, uid)
}

// isDrainedLine: "<timestamp> AGENTDEV_DRAINED ..." (the sidecar prints nothing else on that line).
func isDrainedLine(line string) bool {
	_, rest, ok := cutTimestamp(line)
	return ok && (rest == drainedPrefix || len(rest) > len(drainedPrefix) && rest[:len(drainedPrefix)+1] == drainedPrefix+" ")
}

func cutTimestamp(line string) (time.Time, string, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			t, err := time.Parse(time.RFC3339Nano, line[:i])
			if err != nil {
				return time.Time{}, "", false
			}
			r := line[i+1:]
			for len(r) > 0 && (r[len(r)-1] == '\n' || r[len(r)-1] == '\r') {
				r = r[:len(r)-1]
			}
			return t, r, true
		}
	}
	return time.Time{}, "", false
}

// drainMax: how long the operator waits for running jobs before it replaces the pod anyway (default 60 min).
func (d Defaults) drainMax() time.Duration {
	if n, err := strconv.Atoi(d.DrainMaxMinutes); err == nil && n >= 0 && n <= 24*60 {
		return time.Duration(n) * time.Minute
	}
	return 60 * time.Minute
}

// rollout replaces the agent's pod when it does not run the StatefulSet's current template. Returns how long to wait
// before the next check (0: nothing pending) and the Updated condition for the Agent's status.
func (r *AgentReconciler) rollout(ctx context.Context, a *v1.Agent, sts *appsv1.StatefulSet) (time.Duration, metav1.Condition, error) {
	upToDate := metav1.Condition{Type: UpdatedCondition, Status: metav1.ConditionTrue, Reason: "Current",
		Message: "the pod runs the current template", ObservedGeneration: a.Generation}
	want := sts.Status.UpdateRevision
	if want == "" { // the StatefulSet controller has not computed it yet
		return 0, upToDate, nil
	}
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: sts.Name + "-0"}, &pod)
	if apierrors.IsNotFound(err) {
		return 0, upToDate, nil // stopped (scale to zero, paused): the next start uses the new template
	}
	if err != nil {
		return 0, upToDate, err
	}
	if pod.DeletionTimestamp != nil {
		return 5 * time.Second, metav1.Condition{Type: UpdatedCondition, Status: metav1.ConditionFalse, Reason: "Replacing",
			Message: "the old pod is stopping", ObservedGeneration: a.Generation}, nil
	}
	if pod.Labels[appsv1.ControllerRevisionHashLabelKey] == want {
		if r.Drains != nil {
			r.Drains.Forget(pod.UID)
		}
		return 0, upToDate, nil
	}
	l := log.FromContext(ctx).WithValues("agent", a.Name, "pod", pod.Name)
	now := r.now()
	replace := func(reason, msg string) (time.Duration, metav1.Condition, error) {
		l.Info("replacing the agent pod with the new template", "reason", reason)
		if err := r.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID}); client.IgnoreNotFound(err) != nil {
			return 0, upToDate, err
		}
		r.Drains.Forget(pod.UID)
		return 5 * time.Second, metav1.Condition{Type: UpdatedCondition, Status: metav1.ConditionFalse, Reason: reason,
			Message: msg, ObservedGeneration: a.Generation}, nil
	}
	if pod.Status.Phase != corev1.PodRunning || !podReady(&pod) {
		return replace("NotRunning", "the old pod did not run: replaced at once")
	}
	since, marked := drainSince(&pod)
	if !marked {
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[drainAnnotation] = strconv.FormatInt(now.Unix(), 10)
		if err := r.Patch(ctx, &pod, patch); err != nil {
			return 0, upToDate, fmt.Errorf("mark pod for drain: %w", err)
		}
		l.Info("draining the agent before its pod is replaced (no new jobs; running jobs finish)")
		since = now
	}
	if r.Drains.Drained(pod.UID) {
		return replace("Drained", "the running jobs ended: replaced")
	}
	max := r.D.drainMax()
	if waited := now.Sub(since); waited >= max {
		return replace("Forced", fmt.Sprintf("jobs still running after %s: replaced, work in progress saved on the state volume", max))
	}
	return 15 * time.Second, metav1.Condition{Type: UpdatedCondition, Status: metav1.ConditionFalse, Reason: "Draining",
		Message: fmt.Sprintf("waiting for running jobs to end (since %s, at most %s)", since.UTC().Format(time.RFC3339), max),
		ObservedGeneration: a.Generation}, nil
}

func drainSince(p *corev1.Pod) (time.Time, bool) {
	v, ok := p.Annotations[drainAnnotation]
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false // not ours: set it again
	}
	return time.Unix(n, 0), true
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *AgentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
