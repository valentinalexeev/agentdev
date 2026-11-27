package main

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Ready condition (#386), read by Flux health checks (kstatus) and the Argo CD health checks of the deployment
// template. Ready means "the spec is applied and the router can wake the agent", not "the pod runs": with scale to
// zero most agents are stopped most of the day, and an engine must not report that as a failure.

// agentReady maps an Agent phase to its Ready condition. reconciled is the Reconciled condition status for the
// current generation; reason and msg explain a False.
func agentReady(phase string, reconciled bool, reason, msg string, gen int64) metav1.Condition {
	c := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: gen}
	switch {
	case !reconciled:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, reason, msg
	case phase == "running", phase == "starting", phase == "stopped", phase == "paused":
		c.Reason, c.Message = "Reconciled", "spec applied, agent "+phase
	case phase == "blocked":
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Blocked", "agentdev/blocked on the StatefulSet: never restarted"
	default:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Unknown", "phase "+phase
	}
	if c.Reason == "" {
		c.Reason = "NotReconciled"
	}
	return c
}

// projectSecretReady: ready is True; pending and error are False with the phase as the reason.
func projectSecretReady(phase, msg string, gen int64) metav1.Condition {
	c := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: gen, Message: msg}
	switch phase {
	case "ready":
		c.Status, c.Reason = metav1.ConditionTrue, "Synced"
	case "pending":
		c.Reason = "Pending"
	default:
		c.Reason = "Error"
	}
	if c.Message == "" {
		c.Message = "phase " + phase
	}
	return c
}
