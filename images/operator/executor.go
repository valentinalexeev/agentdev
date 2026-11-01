package main

// Deterministic executors (#69, docs/security-model.md phase 5). The ExecutorPoller is a manager.Runnable that, every
// EXECUTOR_POLL_SECONDS, walks each Executor (namespace EXEC_NS) through this flow, without any model in the loop:
//
//  1. open pull request into spec.branch that changes spec.path: an ExecutorRun stage=pr (plan of the head commit,
//     read-only credentials, sandboxed). Commit status agentdev/trusted-plan; the plan output as a PR comment.
//  2. merged pull request: an ExecutorRun stage=merge (plan of the merge commit, fetched from spec.branch only). The
//     operator's own helper container hashes the plan file; on success an Approval (#67, source executor) bound to
//     executor, environment, commit, plan hash, image digest and Executor spec hash is created on the PR.
//  3. that Approval allowed by a person, with the content hash the operator computed, before its deadline (spec.
//     approvalHours): an ExecutorRun stage=apply, once (the run's name is the single-use marker), started at most
//     EXECUTOR_APPLY_START_MINUTES after the decision and only while the Executor spec still has the approved hash.
//     Its Job verifies the plan file hash before the tool starts, and only this Job gets the write credentials.
//
// One executor per environment: of several Executors with the same spec.environment the oldest runs, the others
// are invalid. Pull requests from forks are planned only with spec.allowForkPRs.
//
// Everything the operator takes from the forge is checked against a pattern before use (commit ^[0-9a-f]{40}$, PR
// numbers >= 1); image, command, branch and path come from the Executor only. Every run start and result is a
// SecurityEvent (source executor; rules executor.plan, executor.apply, executor.refused).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentdev.dev,resources=executors,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentdev.dev,resources=executors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentdev.dev,resources=executorruns;executorruns/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentdev.dev,resources=approvals,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

const (
	planHashPrefix = "AGENTDEV_PLAN_SHA256 "
	planDir        = "/plan"
	planFile       = planDir + "/plan.out"
	workDir        = "/work"
	maxOutput      = 60000 // bytes of tool output posted to the PR
)

var (
	hashRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[a-f0-9]{64}$`) // same as the CRD pattern
)

// ExecConfig holds the operator-wide executor settings (env on the operator, see main.go).
type ExecConfig struct {
	NS          string        // executors, runs and Jobs (EXEC_NS)
	AgentsNS    string        // Approvals and SecurityEvents (AGENTS_NS), where the API watches them
	HelperImage string        // the operator's own image: hash-plan and verify-plan (OPERATOR_IMAGE)
	GitImage    string        // checkout: the forge sidecar image (git, sh)
	PullPolicy  string        //
	ForgeURL    string        // Gitea base URL (API and git)
	GitHubAPI   string        // GitHub REST base (default https://api.github.com)
	GitHubURL   string        // GitHub git base (default https://github.com)
	Poll        time.Duration // EXECUTOR_POLL_SECONDS
	Lookback    time.Duration // merged PRs older than this are never planned (and never before the Executor existed)
	Retention   time.Duration // finished ExecutorRuns (and their Jobs and plan volumes) older than this are deleted
	Storage     string        // plan volume size
	ApplyStart  time.Duration // an allowed apply starts at most this long after the decision (EXECUTOR_APPLY_START_MINUTES)
}

// podLogs returns at most limit bytes of a container's log.
type podLogs func(ctx context.Context, ns, pod, container string, limit int64) (string, error)

func kubePodLogs(cs kubernetes.Interface) podLogs {
	return func(ctx context.Context, ns, pod, container string, limit int64) (string, error) {
		rc, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, LimitBytes: &limit}).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, limit))
		return string(b), err
	}
}

type ExecutorPoller struct {
	client.Client
	Direct client.Reader // API reader: no cache (and no list/watch RBAC) on Secrets
	C      ExecConfig
	Logs   podLogs
	Now    func() time.Time
	// Forge builds the forge client of an executor from its token; replaced in tests.
	Forge func(e *v1.Executor, token string) (forgeAPI, error)
}

func (p *ExecutorPoller) defaultForge(e *v1.Executor, token string) (forgeAPI, error) {
	return newRestForge(forgeAPIBase(forgeOf(e), p.C.ForgeURL, p.C.GitHubAPI), e.Spec.Repo, token)
}

func forgeOf(e *v1.Executor) string {
	if e.Spec.Forge == "" {
		return "gitea"
	}
	return e.Spec.Forge
}

func branchOf(e *v1.Executor) string {
	if e.Spec.Branch == "" {
		return "main"
	}
	return e.Spec.Branch
}

// Start implements manager.Runnable.
func (p *ExecutorPoller) Start(ctx context.Context) error {
	if p.Forge == nil {
		p.Forge = p.defaultForge
	}
	t := time.NewTicker(p.C.Poll)
	defer t.Stop()
	for {
		p.PollAll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// PollAll runs one pass over every Executor; one executor's error does not stop the others.
func (p *ExecutorPoller) PollAll(ctx context.Context) {
	var list v1.ExecutorList
	if err := p.Direct.List(ctx, &list, client.InNamespace(p.C.NS)); err != nil {
		log.FromContext(ctx).Error(err, "list executors")
		return
	}
	owner := envOwners(list.Items)
	for i := range list.Items {
		e := &list.Items[i]
		if o := owner[e.Spec.Environment]; o != e.Name {
			p.duplicate(ctx, e, o)
			continue
		}
		if err := p.Poll(ctx, e); err != nil {
			log.FromContext(ctx).Error(err, "executor", "executor", e.Name)
			p.setPhase(ctx, e, "error", "PollFailed", err.Error())
		}
	}
	if err := p.prune(ctx); err != nil {
		log.FromContext(ctx).Error(err, "prune executor runs")
	}
}

// envOwners maps each environment to the Executor that may change it: the oldest (then the first by name).
func envOwners(items []v1.Executor) map[string]string {
	owner := map[string]*v1.Executor{}
	for i := range items {
		e := &items[i]
		o := owner[e.Spec.Environment]
		if o == nil || e.CreationTimestamp.Before(&o.CreationTimestamp) ||
			(e.CreationTimestamp.Equal(&o.CreationTimestamp) && e.Name < o.Name) {
			owner[e.Spec.Environment] = e
		}
	}
	out := map[string]string{}
	for env, e := range owner {
		out[env] = e.Name
	}
	return out
}

// duplicate marks an Executor invalid whose environment already has an executor; nothing of it runs. The refusal
// is recorded once, when the Executor becomes a duplicate.
func (p *ExecutorPoller) duplicate(ctx context.Context, e *v1.Executor, owner string) {
	msg := "environment " + e.Spec.Environment + " already has executor " + owner + " (one executor per environment)"
	if c := meta.FindStatusCondition(e.Status.Conditions, "Ready"); c == nil || c.Reason != "DuplicateEnvironment" || c.Message != msg {
		log.FromContext(ctx).Info("executor refused", "executor", e.Name, "reason", msg)
		if err := p.record(ctx, e, "", e.Name, "executor.refused", "high", "refused executor "+e.Name+": "+msg); err != nil {
			log.FromContext(ctx).Error(err, "record refusal")
		}
	}
	p.setPhase(ctx, e, "invalid", "DuplicateEnvironment", msg)
}

// specHash is the SHA-256 of everything in the Executor spec that decides what an apply does: image digest,
// commands, credentials, repository, branch, path and environment. The approval covers it; an apply refuses to start
// when the Executor no longer has it.
func specHash(e *v1.Executor) string {
	b, _ := json.Marshal(struct {
		Env, Repo, Forge, Branch, Path, Image string
		Plan, Apply                           []string
		PlanCreds, ApplyCreds                 v1.ExecutorCredentials
		ForgeSecret                           string
	}{e.Spec.Environment, e.Spec.Repo, forgeOf(e), branchOf(e), e.Spec.Path, e.Spec.Image, e.Spec.Plan.Command,
		e.Spec.Apply.Command, e.Spec.PlanCredentials, e.Spec.ApplyCredentials, e.Spec.ForgeSecret})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// validExecutor repeats the CRD rules the operator relies on (fail closed if the CRD was changed or bypassed).
func validExecutor(e *v1.Executor) string {
	n := e.Name
	switch {
	case len(n) > 40:
		return "name longer than 40 characters"
	case e.Spec.ForgeSecret != "exec-"+n+"-forge":
		return "spec.forgeSecret must be exec-" + n + "-forge"
	case e.Spec.PlanCredentials.SecretName != "" && e.Spec.PlanCredentials.SecretName != "exec-"+n+"-plan":
		return "spec.planCredentials.secretName must be exec-" + n + "-plan"
	case e.Spec.ApplyCredentials.SecretName != "" && e.Spec.ApplyCredentials.SecretName != "exec-"+n+"-apply":
		return "spec.applyCredentials.secretName must be exec-" + n + "-apply"
	case e.Spec.PlanCredentials.ServiceAccountName != "" && e.Spec.PlanCredentials.ServiceAccountName == e.Spec.ApplyCredentials.ServiceAccountName:
		return "plan and apply must use different service accounts"
	case !imageRE.MatchString(e.Spec.Image):
		return "spec.image must be pinned by digest"
	case !repoRE.MatchString(e.Spec.Repo) || strings.Contains(e.Spec.Repo, ".."):
		return "invalid spec.repo"
	case len(e.Spec.Plan.Command) == 0 || len(e.Spec.Apply.Command) == 0:
		return "spec.plan and spec.apply need a command"
	case strings.HasPrefix(branchOf(e), "-") || strings.Contains(branchOf(e), ".."):
		return "invalid spec.branch"
	case strings.Contains(e.Spec.Path, ".."):
		return "invalid spec.path"
	}
	return ""
}

// Poll runs one pass of the flow for one Executor.
func (p *ExecutorPoller) Poll(ctx context.Context, e *v1.Executor) error {
	if msg := validExecutor(e); msg != "" {
		p.setPhase(ctx, e, "invalid", "Invalid", msg)
		return nil
	}
	var sec corev1.Secret
	if err := p.Direct.Get(ctx, types.NamespacedName{Namespace: p.C.NS, Name: e.Spec.ForgeSecret}, &sec); err != nil {
		return fmt.Errorf("forge secret: %w", err)
	}
	token := strings.TrimSpace(string(sec.Data["FORGE_TOKEN"]))
	if token == "" {
		return fmt.Errorf("forge secret %s has no FORGE_TOKEN", e.Spec.ForgeSecret)
	}
	f, err := p.Forge(e, token)
	if err != nil {
		return err
	}
	// finish what is running before starting new work (results, approvals, applies)
	var runs v1.ExecutorRunList
	if err := p.Direct.List(ctx, &runs, client.InNamespace(p.C.NS), client.MatchingLabels{"agentdev/executor": e.Name}); err != nil {
		return err
	}
	for i := range runs.Items {
		if err := p.syncRun(ctx, e, f, &runs.Items[i]); err != nil {
			return fmt.Errorf("run %s: %w", runs.Items[i].Name, err)
		}
	}
	if e.Spec.Paused {
		p.setPhase(ctx, e, "paused", "Paused", "spec.paused: no new runs")
		return nil
	}
	if err := p.discover(ctx, e, f); err != nil {
		return err
	}
	p.setPhase(ctx, e, "ready", "Polled", "forge polled")
	return nil
}

// discover creates plan runs for new pull request heads and merge commits.
func (p *ExecutorPoller) discover(ctx context.Context, e *v1.Executor, f forgeAPI) error {
	branch := branchOf(e)
	open, err := f.OpenPRs(ctx)
	if err != nil {
		return err
	}
	for _, pr := range open {
		if pr.Base != branch || pr.Number < 1 || !shaRE.MatchString(pr.Head) {
			continue
		}
		if !e.Spec.AllowForkPRs && !strings.EqualFold(pr.HeadRepo, e.Spec.Repo) {
			continue // a fork (or a deleted head repository): its code is not planned unless the admin opted in
		}
		if err := p.ensurePlan(ctx, e, f, "pr", pr.Number, pr.Head); err != nil {
			return err
		}
	}
	merged, err := f.MergedPRs(ctx)
	if err != nil {
		return err
	}
	cut := p.Now().Add(-p.C.Lookback)
	if c := e.CreationTimestamp.Time; c.After(cut) {
		cut = c // an executor never applies history from before it existed
	}
	for _, pr := range merged {
		if pr.Base != branch || pr.Number < 1 || !shaRE.MatchString(pr.Merge) || pr.MergedAt.Before(cut) {
			continue
		}
		if err := p.ensurePlan(ctx, e, f, "merge", pr.Number, pr.Merge); err != nil {
			return err
		}
	}
	return nil
}

func runName(e *v1.Executor, stage, commit string) string {
	return e.Name + "-" + stage + "-" + commit[:12]
}

func (p *ExecutorPoller) ensurePlan(ctx context.Context, e *v1.Executor, f forgeAPI, stage string, pr int64, commit string) error {
	name := runName(e, stage, commit)
	var existing v1.ExecutorRun
	if err := p.Direct.Get(ctx, types.NamespacedName{Namespace: p.C.NS, Name: name}, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	files, err := f.Files(ctx, pr)
	if err != nil {
		return err
	}
	if !touches(files, e.Spec.Path) {
		return nil
	}
	return p.createRun(ctx, e, v1.ExecutorRunSpec{Executor: e.Name, Action: "plan", Stage: stage, Commit: commit, PR: pr})
}

func (p *ExecutorPoller) createRun(ctx context.Context, e *v1.Executor, spec v1.ExecutorRunSpec) error {
	r := &v1.ExecutorRun{ObjectMeta: metav1.ObjectMeta{Name: runName(e, spec.Stage, spec.Commit), Namespace: p.C.NS,
		Labels: map[string]string{"agentdev/executor": e.Name, "agentdev/stage": spec.Stage}}, Spec: spec}
	if err := controllerutil.SetControllerReference(e, r, p.Scheme()); err != nil {
		return err
	}
	if err := p.Create(ctx, r); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // single use: a run of this stage on this commit exists already
		}
		return err
	}
	log.FromContext(ctx).Info("executor run created", "executor", e.Name, "run", r.Name, "stage", spec.Stage, "pr", spec.PR)
	return nil
}

// syncRun moves one run forward: start its Job, collect its result, then (merge) request and follow the approval.
func (p *ExecutorPoller) syncRun(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun) error {
	// the spec is checked again: an ExecutorRun is only ever written by this operator, but never trust it blindly
	if !shaRE.MatchString(r.Spec.Commit) || r.Spec.PR < 1 || r.Spec.Executor != e.Name {
		return p.finish(ctx, e, f, r, "Failed", "invalid run spec", false)
	}
	switch r.Status.Phase {
	case "", "Pending":
		return p.start(ctx, e, f, r)
	case "Running":
		return p.collect(ctx, e, f, r)
	case "Succeeded":
		if r.Spec.Stage == "merge" {
			return p.followApproval(ctx, e, f, r)
		}
	}
	return nil
}

func (p *ExecutorPoller) start(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun) error {
	if e.Spec.Paused {
		return nil
	}
	if r.Spec.Stage == "apply" {
		switch {
		case !hashRE.MatchString(r.Spec.PlanHash):
			return p.finish(ctx, e, f, r, "Failed", "apply run without a plan hash", true)
		case !hashRE.MatchString(r.Spec.SpecHash) || r.Spec.SpecHash != specHash(e):
			// the Executor (image, commands, credentials, ...) changed after the approval: not what was approved
			return p.finish(ctx, e, f, r, "Failed", "executor spec changed since the approval", true)
		case r.Spec.NotAfter == nil || p.Now().After(r.Spec.NotAfter.Time):
			return p.finish(ctx, e, f, r, "Failed", "apply not started within the start window after the decision", true)
		}
	}
	if r.Spec.Stage == "merge" {
		if err := p.ensurePlanVolume(ctx, e, r); err != nil {
			return err
		}
		r.Status.SpecHash = specHash(e) // the spec this plan runs with; the approval and the apply are bound to it
	}
	job := p.renderJob(e, r)
	if err := controllerutil.SetControllerReference(r, job, p.Scheme()); err != nil {
		return err
	}
	if err := p.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	now := metav1.NewTime(p.Now())
	r.Status.Phase, r.Status.StartedAt, r.Status.Message = "Running", &now, "job "+job.Name+" started"
	if err := p.Status().Update(ctx, r); err != nil {
		return err
	}
	rule := "executor.plan"
	if r.Spec.Action == "apply" {
		rule = "executor.apply"
		if err := f.Comment(ctx, r.Spec.PR, fmt.Sprintf("🤖 agentdev executor `%s` (%s): **apply started** on `%s`, plan sha256 `%s` (approval `%s`).",
			e.Name, e.Spec.Environment, r.Spec.Commit, r.Spec.PlanHash, r.Spec.Approval)); err != nil {
			log.FromContext(ctx).Error(err, "comment apply start")
		}
	} else if r.Spec.Stage == "pr" {
		if err := f.SetStatus(ctx, r.Spec.Commit, "pending", "trusted plan running ("+e.Spec.Environment+")"); err != nil {
			log.FromContext(ctx).Error(err, "set status")
		}
	}
	return p.event(ctx, e, r, rule, severityOf(r, true), "start "+r.Spec.Stage+" "+r.Spec.Action+" on "+r.Spec.Commit)
}

func severityOf(r *v1.ExecutorRun, ok bool) string {
	switch {
	case !ok:
		return "medium"
	case r.Spec.Action == "apply":
		return "medium" // a write to an environment: always worth a look in the SIEM
	default:
		return "low"
	}
}

// collect reads a finished Job's result. The plan hash comes only from the helper container (the operator's own
// image, started after the tool exited), never from the tool's output.
func (p *ExecutorPoller) collect(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun) error {
	var job batchv1.Job
	if err := p.Direct.Get(ctx, types.NamespacedName{Namespace: p.C.NS, Name: r.Name}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return p.finish(ctx, e, f, r, "Failed", "job disappeared", true)
		}
		return err
	}
	done, failed := false, false
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			done = true
		case batchv1.JobFailed:
			done, failed = true, true
		}
	}
	if !done {
		return nil
	}
	pod := p.jobPod(ctx, r.Name)
	out := ""
	if pod != "" {
		out, _ = p.Logs(ctx, p.C.NS, pod, "tool", maxOutput)
	}
	if failed {
		return p.report(ctx, e, f, r, "Failed", "the job failed", out)
	}
	if r.Spec.Action == "plan" {
		h := ""
		if pod != "" {
			hl, err := p.Logs(ctx, p.C.NS, pod, "hash", 4096)
			if err != nil {
				return err
			}
			h = parsePlanHash(hl)
		}
		if h == "" {
			return p.report(ctx, e, f, r, "Failed", "no plan hash from the helper container", out)
		}
		r.Status.PlanHash = h
	}
	return p.report(ctx, e, f, r, "Succeeded", "", out)
}

func (p *ExecutorPoller) jobPod(ctx context.Context, job string) string {
	var pods corev1.PodList
	if err := p.Direct.List(ctx, &pods, client.InNamespace(p.C.NS), client.MatchingLabels{"job-name": job}); err != nil {
		return ""
	}
	name := ""
	for _, pd := range pods.Items { // the last finished attempt (backoffLimit 0: normally one)
		if pd.Status.Phase == corev1.PodSucceeded || pd.Status.Phase == corev1.PodFailed {
			name = pd.Name
		}
	}
	return name
}

// parsePlanHash takes the one line "AGENTDEV_PLAN_SHA256 <64 hex>" the helper prints.
func parsePlanHash(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(l), planHashPrefix); ok && hashRE.MatchString(h) {
			return h
		}
	}
	return ""
}

// report posts the result, sets the commit status (pr) and closes the run.
func (p *ExecutorPoller) report(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun, phase, msg, out string) error {
	ok := phase == "Succeeded"
	what := map[string]string{"pr": "trusted plan of the PR head", "merge": "trusted plan of the merge commit", "apply": "apply"}[r.Spec.Stage]
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 agentdev executor `%s` (%s): %s on `%s`: **%s**", e.Name, e.Spec.Environment, what, r.Spec.Commit,
		map[bool]string{true: "succeeded", false: "failed"}[ok])
	if msg != "" {
		fmt.Fprintf(&b, " (%s)", msg)
	}
	if r.Status.PlanHash != "" {
		fmt.Fprintf(&b, "\n\nPlan file sha256: `%s`", r.Status.PlanHash)
	}
	if r.Spec.Stage == "pr" {
		b.WriteString("\n\nThis plan is of unreviewed code and can never be applied. After the merge the executor plans the merge commit again; only that plan can be approved.")
	}
	b.WriteString("\n\n" + quoteOutput(out))
	if err := f.Comment(ctx, r.Spec.PR, b.String()); err != nil {
		log.FromContext(ctx).Error(err, "comment result")
	} else {
		r.Status.Reported = true
	}
	if r.Spec.Stage == "pr" {
		state := map[bool]string{true: "success", false: "failure"}[ok]
		if err := f.SetStatus(ctx, r.Spec.Commit, state, "trusted plan "+map[bool]string{true: "ready", false: "failed"}[ok]+" ("+e.Spec.Environment+")"); err != nil {
			log.FromContext(ctx).Error(err, "set status")
		}
	}
	if err := p.finish(ctx, e, f, r, phase, msg, false); err != nil {
		return err
	}
	rule := "executor.plan"
	if r.Spec.Action == "apply" {
		rule = "executor.apply"
	}
	return p.event(ctx, e, r, rule, severityOf(r, ok), strings.ToLower(phase)+" "+r.Spec.Stage+" "+r.Spec.Action+" on "+r.Spec.Commit)
}

// quoteOutput shows tool output as data: in a fence, every line prefixed, so no line of it starts a forge command
// (for example /approve) or closes the fence. Plan output of a PR comes from unreviewed code.
func quoteOutput(out string) string {
	out = clip(out, maxOutput)
	if strings.TrimSpace(out) == "" {
		return "_(no output)_"
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "│ " + strings.ReplaceAll(l, "`", "'")
	}
	return "<details><summary>output</summary>\n\n```text\n" + strings.Join(lines, "\n") + "\n```\n</details>"
}

func (p *ExecutorPoller) finish(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun, phase, msg string, refused bool) error {
	now := metav1.NewTime(p.Now())
	r.Status.Phase, r.Status.Message, r.Status.FinishedAt = phase, msg, &now
	if err := p.Status().Update(ctx, r); err != nil {
		return err
	}
	if phase != "Succeeded" || r.Spec.Stage == "apply" {
		p.dropPlanVolumeOf(ctx, e, r)
	}
	if refused {
		return p.refuse(ctx, e, r, msg)
	}
	return nil
}

func (p *ExecutorPoller) refuse(ctx context.Context, e *v1.Executor, r *v1.ExecutorRun, msg string) error {
	log.FromContext(ctx).Info("executor refused", "executor", e.Name, "run", r.Name, "reason", msg)
	return p.event(ctx, e, r, "executor.refused", "high", "refused "+r.Spec.Stage+" on "+r.Spec.Commit+": "+msg)
}

// followApproval requests the apply approval for a successful merge plan and starts the apply once it is allowed.
//
// Deadlines (#107 keeps requests decidable without expiry; an executor apply decides its own): a decision counts
// only if made before the request's Expires (spec.approvalHours), and the apply must start within ApplyStart of the
// decision. Later, the merge run closes and nothing is applied; plan again by merging a new commit.
func (p *ExecutorPoller) followApproval(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun) error {
	if r.Status.Message != "" {
		return nil // closed: apply started, denied, void, expired or refused
	}
	if !hashRE.MatchString(r.Status.PlanHash) {
		return p.closeMerge(ctx, e, r, "merge plan without a hash", true)
	}
	if !hashRE.MatchString(r.Status.SpecHash) || r.Status.SpecHash != specHash(e) {
		return p.closeMerge(ctx, e, r, "executor spec changed since the merge plan", true)
	}
	if r.Status.Approval == "" {
		return p.requestApproval(ctx, e, f, r)
	}
	var a v1.Approval
	if err := p.Direct.Get(ctx, types.NamespacedName{Namespace: p.C.AgentsNS, Name: r.Status.Approval}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return p.closeMerge(ctx, e, r, "approval gone", false)
		}
		return err
	}
	want := approvalHash(a.Spec.ID, a.Spec.Agent, a.Spec.Repo, a.Spec.Job, a.Spec.Source, a.Spec.Tool, a.Spec.Input)
	if a.Spec.Agent != e.Name || a.Spec.Source != "executor" || a.Spec.ContentHash != want ||
		a.Spec.Input != approvalInput(e, r) || a.Spec.Job != fmt.Sprintf("pr-%d", r.Spec.PR) {
		return p.closeMerge(ctx, e, r, "approval does not match the merge plan", true)
	}
	now := p.Now()
	switch a.Status.State {
	case "allowed":
		switch {
		case a.Status.ContentHash != a.Spec.ContentHash:
			return p.closeMerge(ctx, e, r, "approval decided for another content hash", true)
		case a.Status.DecidedAt == nil || a.Status.DecidedAt.After(a.Spec.Expires.Time):
			return p.closeMerge(ctx, e, r, "approval decided after its deadline", false)
		case now.After(a.Status.DecidedAt.Add(p.applyStart())):
			// e.g. the operator was down: an old decision does not start an apply on a world that moved on
			return p.closeMerge(ctx, e, r, "approval decided more than "+p.applyStart().String()+" ago, apply not started", false)
		}
		notAfter := metav1.NewTime(a.Status.DecidedAt.Add(p.applyStart()))
		if err := p.createRun(ctx, e, v1.ExecutorRunSpec{Executor: e.Name, Action: "apply", Stage: "apply", Commit: r.Spec.Commit,
			PR: r.Spec.PR, Approval: a.Name, PlanHash: r.Status.PlanHash, SpecHash: r.Status.SpecHash, NotAfter: &notAfter}); err != nil {
			return err
		}
		return p.closeMerge(ctx, e, r, "apply "+runName(e, "apply", r.Spec.Commit)+" by "+clip(a.Status.DecidedBy, 60), false)
	case "denied":
		return p.closeMerge(ctx, e, r, "approval denied", false)
	case "void":
		return p.closeMerge(ctx, e, r, "approval void: "+clip(a.Status.Reason, 100), false)
	}
	if now.After(a.Spec.Expires.Time) {
		return p.closeMerge(ctx, e, r, "approval expired", false)
	}
	return nil
}

func (p *ExecutorPoller) applyStart() time.Duration {
	if p.C.ApplyStart <= 0 {
		return 30 * time.Minute
	}
	return p.C.ApplyStart
}

// closeMerge records the end of the approval phase of a merge run; the plan volume stays only for a started apply.
func (p *ExecutorPoller) closeMerge(ctx context.Context, e *v1.Executor, r *v1.ExecutorRun, msg string, refused bool) error {
	r.Status.Message = msg
	if err := p.Status().Update(ctx, r); err != nil {
		return err
	}
	if !strings.HasPrefix(msg, "apply ") {
		p.dropPlanVolumeOf(ctx, e, r)
	}
	if refused {
		return p.refuse(ctx, e, r, msg)
	}
	return nil
}

// approvalInput is what the person approves: executor, environment, commit, plan hash, image digest, apply argv and
// the Executor spec hash (which also covers the credentials). Built only from checked values (no tool output), so it
// is the same every time it is computed; a changed Executor gives another input and the approval no longer matches.
func approvalInput(e *v1.Executor, r *v1.ExecutorRun) string {
	argv, _ := json.Marshal(e.Spec.Apply.Command)
	return fmt.Sprintf("apply executor %s to environment %s: repository %s, merge commit %s of PR #%d, plan file sha256 %s, "+
		"image %s, apply argv %s, executor spec sha256 %s",
		e.Name, e.Spec.Environment, e.Spec.Repo, r.Spec.Commit, r.Spec.PR, r.Status.PlanHash, e.Spec.Image,
		clip(string(argv), 1500), r.Status.SpecHash)
}

// approvalHash is the content hash of an Approval (#67): SHA-256 over the fields, each followed by a NUL byte. Same
// definition as the forge sidecar's and the API's.
func approvalHash(fields ...string) string {
	h := sha256.New()
	for _, s := range fields {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *ExecutorPoller) requestApproval(ctx context.Context, e *v1.Executor, f forgeAPI, r *v1.ExecutorRun) error {
	self, err := f.SelfID(ctx)
	if err != nil {
		return fmt.Errorf("executor forge account: %w", err)
	}
	var rnd [8]byte
	var sec [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	if _, err := rand.Read(sec[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(rnd[:])
	sh := sha256.Sum256([]byte(hex.EncodeToString(sec[:]))) // the operator reads the status directly; no one gets the secret
	hours := e.Spec.ApprovalHours
	if hours < 1 || hours > 168 {
		hours = 24
	}
	now := p.Now().UTC().Truncate(time.Second)
	spec := v1.ApprovalSpec{ID: id, Agent: e.Name, Repo: e.Spec.Repo, Forge: forgeOf(e), Job: fmt.Sprintf("pr-%d", r.Spec.PR),
		Source: "executor", Tool: "apply", Input: approvalInput(e, r), RequesterID: self, SecretHash: hex.EncodeToString(sh[:]),
		Created: metav1.NewTime(now), Expires: metav1.NewTime(now.Add(time.Duration(hours) * time.Hour))}
	spec.ContentHash = approvalHash(spec.ID, spec.Agent, spec.Repo, spec.Job, spec.Source, spec.Tool, spec.Input)
	a := &v1.Approval{ObjectMeta: metav1.ObjectMeta{Name: "exec-" + id, Namespace: p.C.AgentsNS,
		Labels: map[string]string{"agent": e.Name, "agentdev/source": "executor"}}, Spec: spec}
	if err := p.Create(ctx, a); err != nil {
		return err
	}
	r.Status.Approval = a.Name
	if err := p.Status().Update(ctx, r); err != nil {
		return err
	}
	body := fmt.Sprintf("🤖 agentdev executor `%s`: the merge commit `%s` is planned. **Apply to `%s` needs an approval.**\n\n"+
		"Plan file sha256: `%s`, image `%s`, executor spec sha256 `%s`. Review the plan output above, then a person "+
		"with write access replies `/approve %s` (or `/deny %s`). A decision counts only within %d h, the apply starts "+
		"within %s of it, is single-use, and covers only this plan file and this executor spec.",
		e.Name, r.Spec.Commit, e.Spec.Environment, r.Status.PlanHash, e.Spec.Image, r.Status.SpecHash, id, id, hours, p.applyStart())
	if err := f.Comment(ctx, r.Spec.PR, body); err != nil {
		log.FromContext(ctx).Error(err, "comment approval request")
	}
	return nil
}

// event records a SecurityEvent for an executor run in the agents namespace, where the API copies it to Postgres.
func (p *ExecutorPoller) event(ctx context.Context, e *v1.Executor, r *v1.ExecutorRun, rule, severity, action string) error {
	return p.record(ctx, e, fmt.Sprintf("pr-%d", r.Spec.PR), r.Name, rule, severity, action)
}

// record writes one SecurityEvent; job may be empty (an event about the Executor itself).
func (p *ExecutorPoller) record(ctx context.Context, e *v1.Executor, job, pod, rule, severity, action string) error {
	now := p.Now()
	h := sha256.Sum256([]byte(pod + "\x00" + rule + "\x00" + action + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "executor", Rule: rule, Severity: severity, Agent: e.Name,
		Job: job, Action: clip(action, 503), Time: metav1.NewTime(now), Pod: pod,
		Container: "executor", Hash: hex.EncodeToString(h[:])}
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: p.C.AgentsNS,
		Labels: map[string]string{"agent": e.Name, "agentdev/severity": severity, "agentdev/job": spec.Job, "agentdev/source": "executor"}},
		Spec: spec}
	if err := p.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (p *ExecutorPoller) setPhase(ctx context.Context, e *v1.Executor, phase, reason, msg string) {
	now := metav1.NewTime(p.Now())
	st := e.Status.DeepCopy()
	st.Phase, st.ObservedGeneration, st.LastPoll = phase, e.Generation, &now
	status := metav1.ConditionTrue
	if phase == "invalid" || phase == "error" {
		status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason,
		Message: clip(msg, 300), ObservedGeneration: e.Generation})
	e.Status = *st
	if err := p.Status().Update(ctx, e); err != nil {
		log.FromContext(ctx).V(1).Info("executor status", "err", err.Error())
	}
}

func planVolumeName(e *v1.Executor, commit string) string {
	return runName(e, "merge", commit) + "-plan"
}

// ensurePlanVolume creates the plan volume of a merge run. It is owned by the merge run; the plan Job writes it, the
// apply Job mounts it read-only, and it is deleted once the apply ran or the approval ended.
func (p *ExecutorPoller) ensurePlanVolume(ctx context.Context, e *v1.Executor, r *v1.ExecutorRun) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: planVolumeName(e, r.Spec.Commit), Namespace: p.C.NS,
		Labels: map[string]string{"agentdev/executor": e.Name}},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.C.Storage)}}}}
	if err := controllerutil.SetControllerReference(r, pvc, p.Scheme()); err != nil {
		return err
	}
	if err := p.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (p *ExecutorPoller) dropPlanVolumeOf(ctx context.Context, e *v1.Executor, r *v1.ExecutorRun) {
	if r.Spec.Stage == "pr" {
		return
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: planVolumeName(e, r.Spec.Commit), Namespace: p.C.NS}}
	if err := p.Delete(ctx, pvc); client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Error(err, "delete plan volume", "pvc", pvc.Name)
	}
}

// prune deletes finished runs older than Retention (Jobs and volumes go with them by owner reference). Applies are
// single-use by name: their runs are never deleted before the approval they used can have expired (168 h max).
func (p *ExecutorPoller) prune(ctx context.Context) error {
	var runs v1.ExecutorRunList
	if err := p.Direct.List(ctx, &runs, client.InNamespace(p.C.NS)); err != nil {
		return err
	}
	keep := p.C.Retention
	if keep < 8*24*time.Hour {
		keep = 8 * 24 * time.Hour
	}
	cut := p.Now().Add(-keep)
	for i := range runs.Items {
		r := &runs.Items[i]
		if r.Status.FinishedAt == nil || r.Status.FinishedAt.After(cut) {
			continue
		}
		if err := p.Delete(ctx, r, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// renderJob is the only definition of an executor Job. Image and commands come from the Executor; the commit, PR and
// environment reach the containers as environment values only (no shell string is built from them).
func (p *ExecutorPoller) renderJob(e *v1.Executor, r *v1.ExecutorRun) *batchv1.Job {
	apply := r.Spec.Action == "apply"
	creds, cmd := e.Spec.PlanCredentials, e.Spec.Plan.Command
	if apply {
		creds, cmd = e.Spec.ApplyCredentials, e.Spec.Apply.Command
	}
	ref := fmt.Sprintf("refs/pull/%d/head", r.Spec.PR)
	if r.Spec.Stage != "pr" {
		ref = "refs/heads/" + branchOf(e) // merge and apply: only commits on the protected branch
	}
	gitBase := strings.TrimRight(p.C.ForgeURL, "/")
	gitUser := "token"
	if forgeOf(e) == "github" {
		gitBase, gitUser = strings.TrimRight(p.C.GitHubURL, "/"), "x-access-token"
		if gitBase == "" {
			gitBase = "https://github.com"
		}
	}
	env := func(k, v string) corev1.EnvVar { return corev1.EnvVar{Name: k, Value: v} }
	common := []corev1.EnvVar{env("COMMIT", r.Spec.Commit), env("ENVIRONMENT", e.Spec.Environment), env("PLAN_FILE", planFile),
		env("PR_NUMBER", fmt.Sprint(r.Spec.PR)), env("HOME", workDir+"/home")}
	src := workDir + "/src"
	toolDir := src
	if e.Spec.Path != "" {
		toolDir = src + "/" + e.Spec.Path
	}
	restricted := func(uid int64, roRoot bool) *corev1.SecurityContext {
		return &corev1.SecurityContext{RunAsUser: ptr.To(uid), RunAsGroup: ptr.To[int64](1000), RunAsNonRoot: ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(roRoot),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	}
	mounts := []corev1.VolumeMount{{Name: "work", MountPath: workDir}, {Name: "plan", MountPath: planDir, ReadOnly: apply}}
	checkout := corev1.Container{
		Name: "checkout", Image: p.C.GitImage, ImagePullPolicy: corev1.PullPolicy(p.C.PullPolicy),
		Command: []string{"/bin/sh", "-c", checkoutScript},
		Env: []corev1.EnvVar{env("REPO_URL", gitBase+"/"+e.Spec.Repo+".git"), env("REF", ref), env("COMMIT", r.Spec.Commit),
			env("GIT_USER", gitUser), env("HOME", "/tmp"), env("GIT_TERMINAL_PROMPT", "0"), env("GIT_CONFIG_NOSYSTEM", "1"),
			{Name: "FORGE_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.Spec.ForgeSecret}, Key: "FORGE_TOKEN"}}}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: workDir}, {Name: "tmp", MountPath: "/tmp"}},
		SecurityContext: restricted(1000, true),
		Resources:       res("50m", "128Mi", "1", "512Mi"),
	}
	tool := corev1.Container{
		Name: "tool", Image: e.Spec.Image, ImagePullPolicy: corev1.PullIfNotPresent, Command: cmd, WorkingDir: toolDir,
		Env: common, VolumeMounts: mounts, SecurityContext: restricted(1000, false), Resources: res("100m", "256Mi", "2", "2Gi"),
	}
	if creds.SecretName != "" {
		tool.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: creds.SecretName}}}}
	}
	helper := func(name string, args ...string) corev1.Container {
		return corev1.Container{Name: name, Image: p.C.HelperImage, ImagePullPolicy: corev1.PullPolicy(p.C.PullPolicy), Args: args,
			VolumeMounts: []corev1.VolumeMount{{Name: "plan", MountPath: planDir, ReadOnly: true}}, SecurityContext: restricted(1000, true),
			Resources: res("10m", "32Mi", "500m", "128Mi")}
	}
	var inits, main []corev1.Container
	if apply {
		// the plan is checked before the tool starts; a different file never reaches the tool
		inits = []corev1.Container{checkout, helper("verify", "verify-plan", planFile, r.Spec.PlanHash)}
		main = []corev1.Container{tool}
	} else {
		// the tool runs as an init container, so the hash is taken after it exited and by the operator's own image
		inits = []corev1.Container{checkout, tool}
		main = []corev1.Container{helper("hash", "hash-plan", planFile)}
	}
	planVol := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	if r.Spec.Stage != "pr" {
		planVol = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: planVolumeName(e, r.Spec.Commit), ReadOnly: apply}}
	}
	labels := map[string]string{"app": "agentdev-executor", "agentdev/executor": e.Name, "agentdev/stage": r.Spec.Stage}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: p.C.NS, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To[int32](0), // an apply is never retried by Kubernetes
			ActiveDeadlineSeconds: ptr.To[int64](3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           creds.ServiceAccountName, // empty: default (no rights); GKE: Workload Identity
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					RuntimeClassName:             ptr.To("gvisor"),
					NodeSelector:                 map[string]string{"agentdev/pool": "sandboxes"},
					Tolerations: []corev1.Toleration{
						{Key: "agentdev/pool", Operator: corev1.TolerationOpEqual, Value: "sandboxes", Effect: corev1.TaintEffectNoSchedule},
						{Key: "sandbox.gke.io/runtime", Operator: corev1.TolerationOpEqual, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule},
					},
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), FSGroup: ptr.To[int64](1000),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					InitContainers: inits,
					Containers:     main,
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "plan", VolumeSource: planVol},
					},
				},
			},
		},
	}
}

func res(cpuReq, memReq, cpuLim, memLim string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuReq), corev1.ResourceMemory: resource.MustParse(memReq)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLim), corev1.ResourceMemory: resource.MustParse(memLim)},
	}
}

// checkoutScript fetches REF from REPO_URL and checks out COMMIT. All values come from the environment (set by the
// operator, COMMIT checked against ^[0-9a-f]{40}$); the token is passed as a header, never stored in the checkout,
// and .git is removed so the tool sees only the tree. A COMMIT that is not reachable from REF fails the checkout:
// merge and apply runs fetch the protected branch only, so they can only run commits that are on it. The first fetch
// is shallow (200 commits); a COMMIT deeper on a busy branch is found by fetching the full history of REF (never the
// commit by id, which would accept commits that are not on the branch).
const checkoutScript = `set -eu
case "$COMMIT" in *[!0-9a-f]*) echo "invalid commit" >&2; exit 2;; esac
[ "${#COMMIT}" -eq 40 ] || { echo "invalid commit" >&2; exit 2; }
mkdir -p /work/home
git init -q /work/src
cd /work/src
auth=$(printf '%s:%s' "$GIT_USER" "$FORGE_TOKEN" | base64 | tr -d '\n')
git -c http.extraHeader="Authorization: Basic $auth" -c protocol.allow=never -c protocol.https.allow=always -c protocol.http.allow=always \
  fetch -q --no-tags --depth 200 -- "$REPO_URL" "$REF"
if ! git cat-file -e "$COMMIT^{commit}" 2>/dev/null; then
  git -c http.extraHeader="Authorization: Basic $auth" -c protocol.allow=never -c protocol.https.allow=always -c protocol.http.allow=always \
    fetch -q --no-tags --unshallow -- "$REPO_URL" "$REF"
fi
git -c advice.detachedHead=false checkout -q --detach "$COMMIT"
[ "$(git rev-parse HEAD)" = "$COMMIT" ]
rm -rf .git
`
