package main

// Rolling agent rollout (#176). The operator's agent template has a version: a short hash of its settings (images,
// URLs, limits: everything in Defaults). When the version changes (an upgrade), agents move to it a few at a time:
//
//  1. the canary first (ROLLOUT_CANARY; empty: the first agent by name);
//  2. the health gate: the canary's new pod must be ready and keep all containers running, without a restart, for
//     ROLLOUT_GATE_SECONDS (default 120). The worker exits at start when its forge identity, forge access or Claude
//     fails, so a broken image or setting restarts the container and fails the gate. The check is the operator's own
//     reading of the pod status (kubelet), not anything the model can write (reviewer checklist item 4);
//  3. then the other agents, at most ROLLOUT_MAX_UNAVAILABLE at a time (default 1), each drained first (#174).
//     Agents without a pod (scaled to zero, paused) move at once after the canary passed;
//  4. a failed gate (a restart, or not healthy within ROLLOUT_GATE_TIMEOUT_MINUTES, default 15) stops the rollout:
//     the agent is Failed, every agent not yet moved keeps its old template (Halted). Rollback is the previous
//     settings (revert the deployment PR): that is a new version and the same rollout runs back.
//
// Per agent the state is status.rollout {version, phase, reason}; scripts/rollout-wait.sh and the playbook read it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	versionAnnotation = "agentdev/version" // on the pod template: the template version it was rendered from

	RolloutCurrent  = "Current"  // the agent runs (or will start with) the current version, gate passed
	RolloutUpdating = "Updating" // moved to the current version; the gate is not passed yet
	RolloutWaiting  = "Waiting"  // on an older version; waits for the canary or a free slot
	RolloutFailed   = "Failed"   // the gate failed on this agent; the rollout stopped
	RolloutHalted   = "Halted"   // on an older version; the rollout stopped because another agent failed
)

// RolloutConfig: the knobs of the rollout (ROLLOUT_* environment of the operator).
type RolloutConfig struct {
	Canary         string
	MaxUnavailable int
	Gate           time.Duration
	GateTimeout    time.Duration
}

func (c RolloutConfig) withDefaults() RolloutConfig {
	if c.MaxUnavailable < 1 {
		c.MaxUnavailable = 1
	}
	if c.Gate <= 0 {
		c.Gate = 120 * time.Second
	}
	if c.GateTimeout <= 0 {
		c.GateTimeout = 15 * time.Minute
	}
	return c
}

// templateVersion: the version of the agent template the operator renders now. Every setting in Defaults is in it.
func (d Defaults) templateVersion() string {
	b, _ := json.Marshal(d) // map keys are sorted: stable
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:12]
}

// canaryOf: the configured canary if it is an agent, else the first agent by name.
func (c RolloutConfig) canaryOf(agents []v1.Agent) string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.Name == c.Canary {
			return a.Name
		}
		names = append(names, a.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// mayMove decides whether agent a (whose StatefulSet renders an older version) may take the current version now.
// It reads the rollout status of all agents, which this operator wrote (the only writer of Agent status).
func (r *AgentReconciler) mayMove(ctx context.Context, a *v1.Agent, hasPod bool) (bool, string, string, error) {
	cfg := r.Rollout.withDefaults()
	ver := r.D.templateVersion()
	var list v1.AgentList
	if err := r.List(ctx, &list, client.InNamespace(a.Namespace)); err != nil {
		return false, "", "", err
	}
	canary := cfg.canaryOf(list.Items)
	busy, canaryDone := 0, false
	for _, o := range list.Items {
		st := o.Status.Rollout
		if st == nil || st.Version != ver {
			continue
		}
		switch st.Phase {
		case RolloutFailed:
			return false, RolloutHalted, fmt.Sprintf("the rollout of %s stopped: agent %s failed (%s)", ver, o.Name, st.Reason), nil
		case RolloutUpdating:
			if o.Name != a.Name {
				busy++
			}
		case RolloutCurrent:
			if o.Name == canary {
				canaryDone = true
			}
		}
	}
	switch {
	case a.Name == canary:
		return true, "", "", nil
	case !canaryDone:
		return false, RolloutWaiting, fmt.Sprintf("waits for the canary %s", canary), nil
	case !hasPod:
		return true, "", "", nil // nothing to drain or check now: the next start uses the new template
	case busy >= cfg.MaxUnavailable:
		return false, RolloutWaiting, fmt.Sprintf("waits for a free slot (%d of %d agents updating)", busy, cfg.MaxUnavailable), nil
	}
	return true, "", "", nil
}

// gate checks an agent whose template is at the current version. Returns the phase (Current, Updating, Failed) and
// a reason. Sticky: a Failed agent stays Failed for this version.
func (r *AgentReconciler) gate(ctx context.Context, a *v1.Agent, sts *appsv1.StatefulSet, isCanary bool) (string, string, time.Duration, error) {
	cfg := r.Rollout.withDefaults()
	ver := r.D.templateVersion()
	if st := a.Status.Rollout; st != nil && st.Version == ver {
		if st.Phase == RolloutFailed || st.Phase == RolloutCurrent {
			return st.Phase, st.Reason, 0, nil // decided for this version; Current is not taken back
		}
	}
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: sts.Name + "-0"}, &pod)
	if apierrors.IsNotFound(err) {
		// no pod: a stopped agent has nothing to check now. The canary is the exception: the gate needs a pod, so the
		// reconciler starts it once (the idle timer stops it again), unless it is paused or blocked.
		// A pod that cannot start (unschedulable, image pull) exists as Pending and fails the gate timeout below.
		if isCanary && rolloutPending(a, ver) && !a.Spec.Paused && sts.Annotations["agentdev/blocked"] != "true" {
			return RolloutUpdating, "starting the canary for the health gate", 10 * time.Second, nil
		}
		return RolloutCurrent, "no pod: the next start uses the new template", 0, nil
	}
	if err != nil {
		return "", "", 0, err
	}
	// the pod must carry this version itself: the StatefulSet's update revision can lag behind a template change
	if pod.Annotations[versionAnnotation] != ver || sts.Status.UpdateRevision == "" ||
		pod.Labels[appsv1.ControllerRevisionHashLabelKey] != sts.Status.UpdateRevision || pod.DeletionTimestamp != nil {
		return RolloutUpdating, "draining the old pod (#174)", 15 * time.Second, nil
	}
	for _, c := range pod.Status.ContainerStatuses {
		if c.RestartCount > 0 {
			why := "restarted"
			if t := c.LastTerminationState.Terminated; t != nil {
				why = fmt.Sprintf("exited %d (%s)", t.ExitCode, t.Reason)
			}
			return RolloutFailed, fmt.Sprintf("container %s of the new pod %s", c.Name, why), 0, nil
		}
	}
	now := r.now()
	started := pod.CreationTimestamp.Time
	if !podReady(&pod) {
		if now.Sub(started) > cfg.GateTimeout {
			return RolloutFailed, fmt.Sprintf("the new pod is not ready after %s", cfg.GateTimeout), 0, nil
		}
		return RolloutUpdating, "waiting for the new pod to be ready", 10 * time.Second, nil
	}
	readySince := started
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			readySince = c.LastTransitionTime.Time
		}
	}
	if left := cfg.Gate - now.Sub(readySince); left > 0 {
		return RolloutUpdating, fmt.Sprintf("health gate: ready since %s, %s to go", readySince.UTC().Format(time.RFC3339), left.Round(time.Second)), left, nil
	}
	return RolloutCurrent, "health gate passed", 0, nil
}

// rolloutPending: an upgrade of this agent to version ver is in progress (it was on another version, or moved and has
// not passed or failed the gate yet). A new agent, or one from before #176 (no rollout status), is not an upgrade.
func rolloutPending(a *v1.Agent, ver string) bool {
	st := a.Status.Rollout
	return st != nil && (st.Version != ver || st.Phase == RolloutUpdating)
}

// templateVersionOf: the version the StatefulSet's template was rendered from ("" before #176).
func templateVersionOf(s *appsv1.StatefulSet) string { return s.Spec.Template.Annotations[versionAnnotation] }

func envDuration(k string, unit time.Duration, def time.Duration) time.Duration {
	if n, err := strconv.Atoi(env(k, "")); err == nil && n > 0 {
		return time.Duration(n) * unit
	}
	return def
}
