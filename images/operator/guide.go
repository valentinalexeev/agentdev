package main

// Harness guide (#206): every agent starts each Claude run with a short text that says it runs under agentdev, what
// it does not have and what the harness gives it, so it uses the harness from the first turn instead of guessing.
//
// The operator is the trusted source: it writes the guide from its own inputs (the Agent, its defaults, the Executors
// of the agent's repository), never from repository content or model output. Every value passes a pattern check before
// it is written; a value that fails is left out, not escaped. The text goes to one ConfigMap, agentdev-guides, key =
// agent name. The agent pod mounts its own key read-only (render.go); the loop appends it to the system prompt of every
// run, after the security rules (images/worker/agent-loop.sh). A project-editable section comes with the Project
// object of #54; it is appended, it never replaces this text.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	guidesCM = "agentdev-guides" // one key per agent; the agent pod mounts only its own key
	// guideMaxBytes caps the guide: it is in the context of every run. Lists are cut before this is reached. With the
	// defaults a guide is about 2 KB, so the one ConfigMap (1 MiB limit) holds several hundred agents; at the cap, about
	// 170. Beyond that, split into one ConfigMap per agent (tracked in the docs, admin "See the harness guide").
	guideMaxBytes = 6000
	guideMaxHosts = 40
	guideMaxExecs = 20
)

// guideStatic is the same for every agent. It is versioned with the operator image; admins cannot edit it, so no
// configuration can tell agents that working around the harness is fine.
const guideStatic = `# agentdev harness guide

You run under agentdev, in a sandboxed pod (gVisor), as agent %s. This text comes from the agentdev operator, not from
the repository. It describes what the harness gives you and what it does not.

What you do not have, so do not look for it:
- No credentials: no forge token, cloud key, Kubernetes token or Claude credential is in your environment or files.
- No push: you commit; the harness pushes your commits to branches agent/%s/* and a person merges.
- No direct internet: traffic goes through an egress proxy that allows only the hosts listed below. A refused host is
  reported as a security event. Do not try other routes; say in your summary which host you need.
- No SSH, no cluster access, no apply of infrastructure changes.

How to get things done:
- Code changes: edit and commit in the working tree. Run the project's tests and linters if you can.
- Infrastructure changes: write the code and the plan request in a PR. An executor plans it, a person approves, the
  executor applies. You never apply.
- A tool you are not allowed to use: stop and explain what you need and why. Do not work around the tool list.
- When you are blocked, say exactly what is missing (a host, a tool, a decision) in your summary.
`

var (
	guideHostRe = regexp.MustCompile(`^\.?([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)*[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	guideWordRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	guidePathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	guideRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// guideText renders the guide of agent a from trusted operator inputs only.
func (r *AgentReconciler) guideText(a *v1.Agent, execs []v1.Executor) string {
	name := a.Name
	if !guideWordRe.MatchString(name) {
		name = "(unnamed)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, guideStatic, name, name)

	b.WriteString("\nThis agent:\n")
	if guideRepoRe.MatchString(a.Spec.Repo) {
		fmt.Fprintf(&b, "- Repository: %s.\n", a.Spec.Repo)
	}
	forge := a.Spec.Forge
	if forge == "" {
		forge = "gitea"
	}
	if forge == "github" || forge == "gitea" {
		fmt.Fprintf(&b, "- Forge: %s. Pushes and comments go through the forge sidecar, which holds the credentials.\n", forge)
	}
	if guideWordRe.MatchString(r.D.Reviewer) {
		fmt.Fprintf(&b, "- Reviewer: %s approves and merges your pull requests.\n", r.D.Reviewer)
	}
	if r.D.approvals() == "true" {
		b.WriteString("- Approvals are on: a tool outside your allow-list asks a project member first. Wait for the decision; a refusal is final for this run.\n")
	} else {
		b.WriteString("- Approvals are off: a tool outside your allow-list is refused.\n")
	}
	if r.D.ClaudeProxyURL != "" {
		b.WriteString("- Model calls go through the agentdev credential proxy, which has a token budget per agent.\n")
	}

	var hosts []string
	for _, h := range strings.Fields(r.D.AgentEgressHosts) {
		if guideHostRe.MatchString(h) && len(hosts) < guideMaxHosts {
			hosts = append(hosts, h)
		}
	}
	switch {
	case r.D.EgressProxy == "":
		b.WriteString("- Egress: no proxy is configured for this instance.\n")
	case len(hosts) == 0:
		b.WriteString("- Egress: only in-cluster services; no outside host is allowed.\n")
	default:
		fmt.Fprintf(&b, "- Egress allowed to (a leading dot means the domain and its subdomains): %s.\n", strings.Join(hosts, " "))
	}

	var lines []string
	for _, e := range execs {
		if e.Spec.Repo != a.Spec.Repo || e.Spec.Paused || !guideWordRe.MatchString(e.Spec.Environment) {
			continue
		}
		path := e.Spec.Path
		if path == "" {
			path = "."
		}
		branch := e.Spec.Branch
		if branch == "" {
			branch = "the default branch"
		}
		if !guidePathRe.MatchString(path) || strings.Contains(path, "..") || !(branch == "the default branch" || guideWordRe.MatchString(branch)) {
			continue
		}
		lines = append(lines, fmt.Sprintf("  - %s: changes under %s, applied after merge to %s.", e.Spec.Environment, path, branch))
	}
	sort.Strings(lines)
	if len(lines) > guideMaxExecs {
		lines = lines[:guideMaxExecs]
	}
	if len(lines) > 0 {
		b.WriteString("- Executors for this repository (they plan on the PR and apply after approval; you only write the code):\n")
		b.WriteString(strings.Join(lines, "\n") + "\n")
	} else {
		b.WriteString("- No executor acts on this repository: infrastructure changes are not applied automatically.\n")
	}

	out := b.String()
	if len(out) > guideMaxBytes { // lists are capped above; this is the last line of defence
		out = out[:guideMaxBytes]
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i+1]
		}
	}
	return out
}

// executorsFor lists the Executors whose repository is a's (EXEC_NS); none when executors are off.
func (r *AgentReconciler) executorsFor(ctx context.Context, a *v1.Agent) ([]v1.Executor, error) {
	if r.D.ExecNS == "" {
		return nil, nil
	}
	var l v1.ExecutorList
	if err := r.List(ctx, &l, client.InNamespace(r.D.ExecNS)); err != nil {
		return nil, err
	}
	var out []v1.Executor
	for _, e := range l.Items {
		if e.Spec.Repo == a.Spec.Repo {
			out = append(out, e)
		}
	}
	return out, nil
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,resourceNames=agentdev-guides

// writeGuide sets (text != "") or removes (text == "") the agent's key in agentdev-guides. The ConfigMap is created on
// first use; it is read by name only (the Role grants get/update on this name, create without a name).
func (r *AgentReconciler) writeGuide(ctx context.Context, ns, agent, text string) error {
	return r.writeGuideKey(ctx, ns, agent, text)
}

// writeGuideKey sets or removes one key of agentdev-guides: <agent> (the guide) or <agent>.npmrc (#248). Agent names
// are DNS labels without dots, so the two kinds of keys never collide.
func (r *AgentReconciler) writeGuideKey(ctx context.Context, ns, agent, text string) error {
	var cm corev1.ConfigMap
	rd := r.Direct
	if rd == nil {
		rd = r.Client
	}
	err := rd.Get(ctx, types.NamespacedName{Namespace: ns, Name: guidesCM}, &cm)
	if apierrors.IsNotFound(err) {
		if text == "" {
			return nil
		}
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: guidesCM, Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/part-of": "agentdev"}}, Data: map[string]string{agent: text}}
		return r.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	if text == "" {
		if _, ok := cm.Data[agent]; !ok {
			return nil
		}
		delete(cm.Data, agent)
	} else {
		if cm.Data[agent] == text {
			return nil
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[agent] = text
	}
	return r.Update(ctx, &cm)
}
