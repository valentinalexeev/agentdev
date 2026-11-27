package main

// Retry of infrastructure kills (#593). A job whose pod is stopped by the infrastructure continues on the next pod
// (#175) and is retried there, resuming from the work the loop pushed at the stop, at most spec.retries.infrastructure
// times. The loop makes the decision (images/worker/agent-loop.sh retry_decision, the same rule as retryDecision
// here, kept equal by TestRetryReasonsMatchWorker), from the reason it recorded at SIGTERM. The operator adds what
// only it can see: why the pod stopped, from the pod's conditions and its node, recorded in agentdev-usage (key
// stop.<agent>, mounted into the agent pod already) as "<reason> <unix seconds>". The loop uses it to refine a plain
// pod-deleted (node-preempted, drain, scale-down, upgrade, evicted). Nothing here starts or deletes a pod.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get

const (
	defaultInfraRetries = 2
	stopKeyPrefix       = "stop."
	// the taints of a node that is going away: GKE Spot/preemptible shutdown, cluster autoscaler scale-down, and the
	// one kubectl drain (and the GKE upgrade) sets
	taintGKESpotPreempt     = "cloud.google.com/impending-node-termination"
	taintAutoscalerDelete   = "ToBeDeletedByClusterAutoscaler"
	taintUnschedulableDrain = "node.kubernetes.io/unschedulable"
	labelGKESpot            = "cloud.google.com/gke-spot"
	labelGKEPreemptible     = "cloud.google.com/gke-preemptible"
)

// infraRetryReasons: the stop reasons that are retried (the loop's INFRA_RETRY_REASONS). Everything else, above all
// security-pause and deadline and a run's own error (oom, unknown), is never retried.
var infraRetryReasons = []string{"pod-deleted", "node-preempted", "drain", "scale-down", "upgrade"}

// infraRetries: spec.retries.infrastructure, default 2, clamped to 0..10 like the CRD
func infraRetries(a *v1.Agent) int {
	if a == nil || a.Spec.Retries == nil || a.Spec.Retries.Infrastructure == nil {
		return defaultInfraRetries
	}
	return max(0, min(10, int(*a.Spec.Retries.Infrastructure)))
}

// retryable reports whether a stop reason is an infrastructure kill.
func retryable(reason string) bool {
	for _, r := range infraRetryReasons {
		if r == reason {
			return true
		}
	}
	return false
}

// RetryDecision is what happens to a job whose attempt (1 = the first run) was stopped for reason.
type RetryDecision string

const (
	Retry     RetryDecision = "retry"
	NoRetry   RetryDecision = "no-retry"
	Exhausted RetryDecision = "exhausted"
)

// retryDecision: retry an infrastructure kill while the next attempt is within 1 + retries; never anything else.
// attempt is the number of the run that comes next (2 after the first stop).
func retryDecision(reason string, attempt, retries int) RetryDecision {
	if !retryable(reason) {
		return NoRetry
	}
	if attempt <= retries+1 {
		return Retry
	}
	return Exhausted
}

// stopReasonOf names why an agent pod that is going away stops, from what the cluster shows: the operator's own mark
// (security-pause), its own upgrade (the drain annotation), the DisruptionTarget pod condition, and the node's
// taints and labels. Empty when nothing names a cause (the loop then keeps pod-deleted).
func stopReasonOf(p *corev1.Pod, node *corev1.Node) string {
	if p == nil {
		return ""
	}
	if r := p.Annotations[stopReasonAnnotation]; r != "" {
		return r // security-pause, set by the collector before the pause
	}
	if p.Status.Reason == "Evicted" {
		return "evicted"
	}
	if _, ok := p.Annotations[drainAnnotation]; ok {
		return "upgrade" // the operator replaces the pod with a new template (#174)
	}
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.DisruptionTarget || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case corev1.PodReasonTerminationByKubelet: // graceful node shutdown: what a Spot preemption looks like
			return "node-preempted"
		case "EvictionByEvictionAPI": // kubectl drain, a node upgrade, the autoscaler: the node tells which
			if r := nodeReason(node); r != "" {
				return r
			}
			return "drain"
		case corev1.PodReasonPreemptionByScheduler:
			return "node-preempted"
		case "DeletionByTaintManager":
			if r := nodeReason(node); r != "" {
				return r
			}
			return "drain"
		}
	}
	return nodeReason(node)
}

// nodeReason: the cause the node of the pod shows by its taints, or "" for a node that is not going away
func nodeReason(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	spot := n.Labels[labelGKESpot] == "true" || n.Labels[labelGKEPreemptible] == "true"
	for _, t := range n.Spec.Taints {
		switch t.Key {
		case taintGKESpotPreempt:
			return "node-preempted"
		case taintAutoscalerDelete:
			return "scale-down"
		case taintUnschedulableDrain:
			return "drain"
		}
	}
	if n.Spec.Unschedulable {
		return "drain"
	}
	for _, c := range n.Status.Conditions {
		// a Spot node that stops being ready while the pod goes: the VM was reclaimed
		if spot && c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return "node-preempted"
		}
	}
	return ""
}

// noteStops records, for each agent pod that is being deleted, why (stopReasonOf). Called from the collector's scan
// (every Rescan): a Spot VM has 30 s, so the record is written while the pod terminates and the next pod reads it.
// Best effort: a failure is logged and the loop keeps pod-deleted, which is retried all the same.
func (c *Collector) noteStops(ctx context.Context, pods []corev1.Pod) {
	if c.Usage == nil {
		return
	}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil || p.Labels["app"] != "coding-agent" {
			continue
		}
		var node *corev1.Node
		if p.Spec.NodeName != "" {
			var n corev1.Node
			if err := c.Direct.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &n); err == nil {
				node = &n
			} else if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
				log.FromContext(ctx).V(1).Info("read the node of a stopping agent pod", "node", p.Spec.NodeName, "err", err.Error())
			}
		}
		r := stopReasonOf(p, node)
		if r == "" {
			continue
		}
		at := p.DeletionTimestamp.Time
		if err := c.Usage.noteStop(ctx, agentOf(p), r, at); err != nil {
			log.FromContext(ctx).Error(err, "record the stop reason of an agent pod", "pod", p.Name)
			continue
		}
		if c.Recorder != nil && c.markNoted(string(p.UID)) {
			var a v1.Agent
			if err := c.Direct.Get(ctx, types.NamespacedName{Namespace: c.NS, Name: agentOf(p)}, &a); err == nil {
				c.Recorder.Event(&a, corev1.EventTypeNormal, "PodStopped",
					fmt.Sprintf("pod %s stopped by %s; its jobs continue on the next pod (at most %d retries per job)", p.Name, r, infraRetries(&a)))
			}
		}
	}
}

// markNoted: true the first time a pod UID is seen (one Event per stopped pod)
func (c *Collector) markNoted(uid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noted == nil {
		c.noted = map[string]time.Time{}
	}
	if _, ok := c.noted[uid]; ok {
		return false
	}
	now := c.now()
	for k, t := range c.noted {
		if now.Sub(t) > time.Hour {
			delete(c.noted, k)
		}
	}
	c.noted[uid] = now
	return true
}

// noteStop writes stop.<agent> = "<reason> <unix seconds>" into agentdev-usage (the ConfigMap the agent pod mounts).
// The usage state is kept as it is; Evaluate writes its own keys and keeps the stop keys (writeLocked).
func (u *Usage) noteStop(ctx context.Context, agent, reason string, at time.Time) error {
	if agent == "" || strings.ContainsAny(reason, " \n") {
		return nil
	}
	v := reason + " " + strconv.FormatInt(at.Unix(), 10)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stops == nil {
		u.stops = map[string]string{}
	}
	if u.stops[agent] == v {
		return nil
	}
	var cm corev1.ConfigMap
	err := u.Direct.Get(ctx, types.NamespacedName{Namespace: u.NS, Name: usageCM}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageCM, Namespace: u.NS}, Data: map[string]string{stopKeyPrefix + agent: v}}
		if err := u.Create(ctx, &cm); err != nil {
			return err
		}
		u.stops[agent] = v
		return nil
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[stopKeyPrefix+agent] = v
	if err := u.Update(ctx, &cm); err != nil {
		return err
	}
	u.stops[agent] = v
	return nil
}
