package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Agents from Git (#58). With agents_source=git the Agent objects live in the instance's deployment repository and
// Flux (or Argo CD) applies them. Rule ("Owner of each field", owner analysis on #58):
//   - an Agent made through the API (annotation agentdev/created-by) stays API-owned: Flux prunes only what it applied;
//   - a Git-owned Agent is read-only here: no delete, no change of maxConcurrent or scaleToZero;
//   - spec.paused is a runtime field and never in the Git manifest, so Flux server-side apply does not own it: pause
//     and resume (the emergency stop) stay allowed. The API writes it with its own field manager.
//   - replicas stay with the router (the StatefulSets are never in Git).

const (
	agentsSourceAPI = "api"
	agentsSourceGit = "git"
	// apiFieldManager is the field manager of every API write to an Agent.
	apiFieldManager = "agentdev-api"
)

// runtimeFields may be written by the API on a Git-owned Agent: they are not in the Git manifest.
var runtimeFields = map[string]bool{"paused": true}

var errGitOwned = &httpErr{http.StatusConflict, "denied",
	"this agent is owned by Git (agents_source: git): change it in the deployment repository; only pause and resume are possible here"}

// agentsSourceFromEnv reads AGENTS_SOURCE: empty = api. Another value stops the API at start (fail closed).
func agentsSourceFromEnv(get func(string) string) (string, error) {
	switch v := strings.TrimSpace(get("AGENTS_SOURCE")); v {
	case "", agentsSourceAPI:
		return agentsSourceAPI, nil
	case agentsSourceGit:
		return agentsSourceGit, nil
	default:
		return "", fmt.Errorf("AGENTS_SOURCE must be api or git, got %q", v)
	}
}

func mustAgentsSource() string {
	s, err := agentsSourceFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return s
}

// gitOwned: in git mode, every Agent that the API did not make.
func (w *writer) gitOwned(u *unstructured.Unstructured) bool {
	return w != nil && w.source == agentsSourceGit && u.GetAnnotations()[createdByAnnotation] == ""
}

// gitOwnedPatch refuses a patch of a Git-owned Agent that touches a field other than the runtime fields.
func (w *writer) gitOwnedPatch(u *unstructured.Unstructured, spec map[string]any) error {
	if !w.gitOwned(u) {
		return nil
	}
	var bad []string
	for k := range spec {
		if !runtimeFields[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return &httpErr{errGitOwned.code, errGitOwned.result, errGitOwned.msg + " (refused: " + strings.Join(bad, ", ") + ")"}
}

// sourceOf is the value of agent.Source for the overview: git for a Git-owned Agent, api otherwise.
func sourceOf(mode string, u *unstructured.Unstructured) string {
	if mode == agentsSourceGit && u.GetAnnotations()[createdByAnnotation] == "" {
		return agentsSourceGit
	}
	return agentsSourceAPI
}

// view is the response entry of an Agent after a write.
func (w *writer) view(u *unstructured.Unstructured) agent {
	a := fromAgentObject(*u)
	a.Source = sourceOf(w.source, u)
	return a
}

// agentPatchOptions: every API write to an Agent uses the field manager agentdev-api (#58).
func agentPatchOptions() metav1.PatchOptions {
	return metav1.PatchOptions{FieldManager: apiFieldManager}
}
