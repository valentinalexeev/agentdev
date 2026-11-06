package main

import (
	"regexp"
	"strings"
)

// Cost labels (#184): service and environment on agent and executor pods, so GKE cost allocation and the billing
// export split costs. From COST_LABELS ("service=agentdev,environment=prod"); GCP label rules (lower case).
var (
	costLabelKey   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	costLabelValue = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
)

// parseCostLabels reads "k=v,k=v"; invalid pairs are dropped (the deployment validates them before).
func parseCostLabels(s string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && costLabelKey.MatchString(k) && costLabelValue.MatchString(v) {
			out[k] = v
		}
	}
	return out
}

// withCostLabels returns own plus the cost labels; own labels (selectors, app) always win.
func withCostLabels(own, cost map[string]string) map[string]string {
	out := make(map[string]string, len(own)+len(cost))
	for k, v := range cost {
		out[k] = v
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}
