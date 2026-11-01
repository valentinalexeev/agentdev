package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	execNS  = "agentdev-executors"
	headSHA = "1111111111111111111111111111111111111111"
	mergeSH = "2222222222222222222222222222222222222222"
	planH   = "3333333333333333333333333333333333333333333333333333333333333333"
	digest  = "hashicorp/terraform@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type fakeForge struct {
	open, merged []pullRequest
	files        map[int64][]string
	statuses     []string
	comments     []string
}

func (f *fakeForge) OpenPRs(context.Context) ([]pullRequest, error)   { return f.open, nil }
func (f *fakeForge) MergedPRs(context.Context) ([]pullRequest, error) { return f.merged, nil }
func (f *fakeForge) Files(_ context.Context, pr int64) ([]string, error) {
	return f.files[pr], nil
}
func (f *fakeForge) SetStatus(_ context.Context, sha, state, d string) error {
	f.statuses = append(f.statuses, sha+" "+state)
	return nil
}
func (f *fakeForge) Comment(_ context.Context, pr int64, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeForge) SelfID(context.Context) (int64, error) { return 42, nil }

type execEnv struct {
	p    *ExecutorPoller
	c    client.Client
	f    *fakeForge
	now  time.Time
	logs map[string]string // container -> log
}

func executor(mut func(*v1.Executor)) *v1.Executor {
	e := &v1.Executor{ObjectMeta: metav1.ObjectMeta{Name: "tf-prod", Namespace: execNS, UID: "uid-exec",
		CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))},
		Spec: v1.ExecutorSpec{Environment: "prod", Repo: "acme/platform", Branch: "main", Path: "infra", Image: digest,
			Plan:             v1.ExecutorCommand{Command: []string{"terraform", "plan", "-out=$(PLAN_FILE)"}},
			Apply:            v1.ExecutorCommand{Command: []string{"terraform", "apply", "$(PLAN_FILE)"}},
			PlanCredentials:  v1.ExecutorCredentials{SecretName: "exec-tf-prod-plan", ServiceAccountName: "exec-tf-prod-plan"},
			ApplyCredentials: v1.ExecutorCredentials{SecretName: "exec-tf-prod-apply", ServiceAccountName: "exec-tf-prod-apply"},
			ForgeSecret:      "exec-tf-prod-forge"}}
	if mut != nil {
		mut(e)
	}
	return e
}

func newExecEnv(t *testing.T, e *v1.Executor, objs ...client.Object) *execEnv {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "exec-tf-prod-forge", Namespace: execNS},
		Data: map[string][]byte{"FORGE_TOKEN": []byte("tok")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, e, sec)...).
		WithStatusSubresource(&v1.Executor{}, &v1.ExecutorRun{}, &v1.Approval{}).Build()
	env := &execEnv{c: c, f: &fakeForge{files: map[int64][]string{}}, now: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		logs: map[string]string{"tool": "Plan: 1 to add\n/approve deadbeefdeadbeef\n```", "hash": planHashPrefix + planH + "\n"}}
	env.p = &ExecutorPoller{Client: c, Direct: c, Now: func() time.Time { return env.now },
		C: ExecConfig{NS: execNS, AgentsNS: ns, HelperImage: "operator:dev", GitImage: "forge:dev", PullPolicy: "Never",
			ForgeURL: "http://gitea:3000", Poll: time.Minute, Lookback: 24 * time.Hour, Retention: 30 * 24 * time.Hour, Storage: "1Gi"},
		Logs: func(_ context.Context, _, _, container string, _ int64) (string, error) {
			return env.logs[container], nil
		},
		Forge: func(*v1.Executor, string) (forgeAPI, error) { return env.f, nil }}
	return env
}

func (x *execEnv) poll(t *testing.T) {
	t.Helper()
	var e v1.Executor
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e); err != nil {
		t.Fatal(err)
	}
	if err := x.p.Poll(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
}

func (x *execEnv) run(t *testing.T, name string) *v1.ExecutorRun {
	t.Helper()
	var r v1.ExecutorRun
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: name}, &r); err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return &r
}

func (x *execEnv) job(t *testing.T, name string) *batchv1.Job {
	t.Helper()
	var j batchv1.Job
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: name}, &j); err != nil {
		t.Fatalf("job %s: %v", name, err)
	}
	return &j
}

// finishJob marks the Job complete (or failed) and adds its pod, as the Job controller would.
func (x *execEnv) finishJob(t *testing.T, name string, ok bool) {
	t.Helper()
	j := x.job(t, name)
	typ, phase := batchv1.JobComplete, corev1.PodSucceeded
	if !ok {
		typ, phase = batchv1.JobFailed, corev1.PodFailed
	}
	j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: typ, Status: corev1.ConditionTrue})
	if err := x.c.Status().Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: execNS, Labels: map[string]string{"job-name": name}},
		Status: corev1.PodStatus{Phase: phase}}
	if err := x.c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func (x *execEnv) events(t *testing.T) []string {
	t.Helper()
	var l v1.SecurityEventList
	if err := x.c.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range l.Items {
		out = append(out, e.Spec.Rule+"/"+e.Spec.Severity)
	}
	return out
}

func (x *execEnv) approval(t *testing.T, name string) *v1.Approval {
	t.Helper()
	var a v1.Approval
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

func (x *execEnv) decide(t *testing.T, name, state string, mut func(*v1.Approval)) {
	t.Helper()
	a := x.approval(t, name)
	now := metav1.NewTime(x.now)
	a.Status = v1.ApprovalStatus{State: state, Scope: "once", DecidedBy: "owner", DecidedVia: "forge", DecidedAt: &now, ContentHash: a.Spec.ContentHash}
	if mut != nil {
		mut(a)
	}
	if err := x.c.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorPRPlan(t *testing.T) {
	x := newExecEnv(t, executor(nil))
	x.f.open = []pullRequest{
		{Number: 7, Head: headSHA, Base: "main"},
		{Number: 8, Head: strings.Repeat("4", 40), Base: "main"},     // does not touch infra/
		{Number: 9, Head: "not-a-sha; rm -rf /", Base: "main"},       // invalid commit: ignored
		{Number: 10, Head: strings.Repeat("5", 40), Base: "release"}, // other branch
	}
	x.f.files[7] = []string{"infra/addons.tf"}
	x.f.files[8] = []string{"docs/readme.md", "infrastructure.md"}
	x.poll(t) // creates the run
	x.poll(t) // starts the job
	name := "tf-prod-pr-" + headSHA[:12]
	r := x.run(t, name)
	if r.Status.Phase != "Running" || r.Spec.Stage != "pr" || r.Spec.Action != "plan" {
		t.Fatalf("run %+v", r.Status)
	}
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs)
	if len(runs.Items) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs.Items))
	}
	j := x.job(t, name)
	ps := j.Spec.Template.Spec
	if *ps.AutomountServiceAccountToken || *ps.RuntimeClassName != "gvisor" || ps.ServiceAccountName != "exec-tf-prod-plan" || *j.Spec.BackoffLimit != 0 {
		t.Fatalf("sandbox: %+v", ps)
	}
	if len(ps.InitContainers) != 2 || ps.InitContainers[1].Name != "tool" || ps.Containers[0].Name != "hash" || ps.Containers[0].Image != "operator:dev" {
		t.Fatal("plan job: tool must be an init container, the hash container the operator image")
	}
	tool := ps.InitContainers[1]
	if tool.Image != digest || tool.EnvFrom[0].SecretRef.Name != "exec-tf-prod-plan" || tool.WorkingDir != "/work/src/infra" {
		t.Fatalf("tool %+v", tool)
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		for _, ef := range c.EnvFrom {
			if strings.Contains(ef.SecretRef.Name, "apply") {
				t.Fatal("apply credentials in a plan job")
			}
		}
	}
	if ps.Volumes[2].EmptyDir == nil {
		t.Fatal("PR plan must not get a persistent plan volume")
	}
	if ref := envVal(ps.InitContainers[0], "REF"); ref != "refs/pull/7/head" {
		t.Fatalf("ref %q", ref)
	}
	x.finishJob(t, name, true)
	x.poll(t)
	r = x.run(t, name)
	if r.Status.Phase != "Succeeded" || r.Status.PlanHash != planH {
		t.Fatalf("status %+v", r.Status)
	}
	if want := []string{headSHA + " pending", headSHA + " success"}; strings.Join(x.f.statuses, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses %v", x.f.statuses)
	}
	last := x.f.comments[len(x.f.comments)-1]
	for _, l := range strings.Split(last, "\n") {
		if strings.HasPrefix(l, "/approve") || l == "```" && strings.Contains(last, "│ ```") {
			t.Fatalf("tool output not quoted: %q", l)
		}
	}
	if !strings.Contains(last, "│ /approve deadbeefdeadbeef") || strings.Contains(last, "│ ```") {
		t.Fatalf("output quoting: %s", last)
	}
	// no approval for a PR plan
	var al v1.ApprovalList
	_ = x.c.List(context.Background(), &al)
	if len(al.Items) != 0 {
		t.Fatal("PR plan must never request an approval")
	}
	if ev := strings.Join(x.events(t), ","); !strings.Contains(ev, "executor.plan/low") {
		t.Fatalf("events %s", ev)
	}
}

func envVal(c corev1.Container, k string) string {
	for _, e := range c.Env {
		if e.Name == k {
			return e.Value
		}
	}
	return ""
}

// mergeToApproval walks a merged PR to its approval request and returns the Approval name.
func mergeToApproval(t *testing.T, x *execEnv) string {
	t.Helper()
	x.f.merged = []pullRequest{{Number: 7, Head: headSHA, Merge: mergeSH, Base: "main", MergedAt: x.now.Add(-time.Hour)}}
	x.f.files[7] = []string{"infra/addons.tf"}
	x.poll(t)
	x.poll(t)
	name := "tf-prod-merge-" + mergeSH[:12]
	j := x.job(t, name)
	if ref := envVal(j.Spec.Template.Spec.InitContainers[0], "REF"); ref != "refs/heads/main" {
		t.Fatalf("merge plans fetch the protected branch only, got %q", ref)
	}
	if j.Spec.Template.Spec.Volumes[2].PersistentVolumeClaim == nil {
		t.Fatal("merge plan needs the plan volume")
	}
	x.finishJob(t, name, true)
	x.poll(t) // result
	x.poll(t) // approval request
	r := x.run(t, name)
	if r.Status.Approval == "" {
		t.Fatalf("no approval: %+v", r.Status)
	}
	return r.Status.Approval
}

func TestExecutorMergeApproveApply(t *testing.T) {
	x := newExecEnv(t, executor(nil))
	an := mergeToApproval(t, x)
	a := x.approval(t, an)
	s := a.Spec
	if s.Source != "executor" || s.Tool != "apply" || s.Agent != "tf-prod" || s.Job != "pr-7" || s.RequesterID != 42 ||
		!strings.Contains(s.Input, mergeSH) || !strings.Contains(s.Input, planH) || !strings.Contains(s.Input, "environment prod") {
		t.Fatalf("approval spec %+v", s)
	}
	if s.ContentHash != approvalHash(s.ID, s.Agent, s.Repo, s.Job, s.Source, s.Tool, s.Input) || an != "exec-"+s.ID {
		t.Fatal("content hash or name")
	}
	if s.Expires.Sub(s.Created.Time) != 24*time.Hour {
		t.Fatalf("expiry %v", s.Expires.Sub(s.Created.Time))
	}
	// pending: nothing happens
	x.poll(t)
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs, client.MatchingLabels{"agentdev/stage": "apply"})
	if len(runs.Items) != 0 {
		t.Fatal("apply before approval")
	}
	x.decide(t, an, "allowed", nil)
	x.poll(t) // creates the apply run
	x.poll(t) // starts it
	name := "tf-prod-apply-" + mergeSH[:12]
	r := x.run(t, name)
	if r.Spec.PlanHash != planH || r.Spec.Approval != an || r.Status.Phase != "Running" {
		t.Fatalf("apply run %+v %+v", r.Spec, r.Status)
	}
	ps := x.job(t, name).Spec.Template.Spec
	if ps.ServiceAccountName != "exec-tf-prod-apply" || ps.Containers[0].EnvFrom[0].SecretRef.Name != "exec-tf-prod-apply" {
		t.Fatal("apply credentials")
	}
	v := ps.InitContainers[1]
	if v.Name != "verify" || v.Image != "operator:dev" || strings.Join(v.Args, " ") != "verify-plan /plan/plan.out "+planH {
		t.Fatalf("verify container %+v", v)
	}
	if !ps.Volumes[2].PersistentVolumeClaim.ReadOnly || !ps.Containers[0].VolumeMounts[1].ReadOnly {
		t.Fatal("apply must mount the plan read-only")
	}
	if strings.Join(ps.Containers[0].Command, " ") != "terraform apply $(PLAN_FILE)" {
		t.Fatal("apply command from the Executor")
	}
	// single use: further polls never create a second apply
	x.poll(t)
	x.finishJob(t, name, true)
	x.poll(t)
	x.poll(t)
	_ = x.c.List(context.Background(), &runs, client.MatchingLabels{"agentdev/stage": "apply"})
	if len(runs.Items) != 1 || runs.Items[0].Status.Phase != "Succeeded" {
		t.Fatalf("apply runs %d", len(runs.Items))
	}
	if ev := strings.Join(x.events(t), ","); !strings.Contains(ev, "executor.apply/medium") {
		t.Fatalf("events %s", ev)
	}
	// deleting the apply run (pruning) does not re-trigger it: the merge run is closed
	_ = x.c.Delete(context.Background(), &runs.Items[0])
	x.poll(t)
	x.poll(t)
	_ = x.c.List(context.Background(), &runs, client.MatchingLabels{"agentdev/stage": "apply"})
	if len(runs.Items) != 0 {
		t.Fatal("apply re-created after the merge run closed")
	}
}

func TestExecutorApprovalRefusals(t *testing.T) {
	cases := map[string]struct {
		state string
		mut   func(*v1.Approval)
		event string
	}{
		"denied":       {"denied", nil, ""},
		"expired":      {"expired", nil, ""},
		"other hash":   {"allowed", func(a *v1.Approval) { a.Status.ContentHash = strings.Repeat("0", 64) }, "executor.refused/high"},
		"late decided": {"allowed", func(a *v1.Approval) { d := metav1.NewTime(a.Spec.Expires.Add(time.Minute)); a.Status.DecidedAt = &d }, ""},
	}
	for n, tc := range cases {
		t.Run(n, func(t *testing.T) {
			x := newExecEnv(t, executor(nil))
			an := mergeToApproval(t, x)
			if n == "late decided" {
				x.now = x.now.Add(25 * time.Hour)
			}
			x.decide(t, an, tc.state, tc.mut)
			x.poll(t)
			x.poll(t)
			var runs v1.ExecutorRunList
			_ = x.c.List(context.Background(), &runs, client.MatchingLabels{"agentdev/stage": "apply"})
			if len(runs.Items) != 0 {
				t.Fatal("apply must not run")
			}
			var pvcs corev1.PersistentVolumeClaimList
			_ = x.c.List(context.Background(), &pvcs)
			if len(pvcs.Items) != 0 {
				t.Fatal("plan volume must be dropped")
			}
			if tc.event != "" && !strings.Contains(strings.Join(x.events(t), ","), tc.event) {
				t.Fatalf("events %v", x.events(t))
			}
		})
	}
}

func TestExecutorTamperedApprovalSpec(t *testing.T) {
	x := newExecEnv(t, executor(nil))
	an := mergeToApproval(t, x)
	// replace the Approval with one for another plan (someone with create rights on approvals)
	a := x.approval(t, an)
	_ = x.c.Delete(context.Background(), a)
	a.ResourceVersion = ""
	a.Spec.Input = strings.ReplaceAll(a.Spec.Input, planH, strings.Repeat("9", 64))
	a.Spec.ContentHash = approvalHash(a.Spec.ID, a.Spec.Agent, a.Spec.Repo, a.Spec.Job, a.Spec.Source, a.Spec.Tool, a.Spec.Input)
	if err := x.c.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	x.decide(t, an, "allowed", nil)
	x.poll(t)
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs, client.MatchingLabels{"agentdev/stage": "apply"})
	if len(runs.Items) != 0 {
		t.Fatal("apply of a mismatching approval")
	}
	if !strings.Contains(strings.Join(x.events(t), ","), "executor.refused/high") {
		t.Fatal("refusal not recorded")
	}
}

func TestExecutorMergeWithoutHashFails(t *testing.T) {
	x := newExecEnv(t, executor(nil))
	x.logs["hash"] = "AGENTDEV_PLAN_SHA256 nothex\n" // the tool cannot write here, but a broken helper must fail closed
	x.f.merged = []pullRequest{{Number: 7, Merge: mergeSH, Base: "main", MergedAt: x.now.Add(-time.Hour)}}
	x.f.files[7] = []string{"infra/a.tf"}
	x.poll(t)
	x.poll(t)
	x.finishJob(t, "tf-prod-merge-"+mergeSH[:12], true)
	x.poll(t)
	x.poll(t)
	r := x.run(t, "tf-prod-merge-"+mergeSH[:12])
	if r.Status.Phase != "Failed" || r.Status.Approval != "" {
		t.Fatalf("status %+v", r.Status)
	}
}

func TestExecutorOldMergesIgnored(t *testing.T) {
	x := newExecEnv(t, executor(func(e *v1.Executor) {
		e.CreationTimestamp = metav1.NewTime(time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC))
	}))
	x.f.merged = []pullRequest{{Number: 7, Merge: mergeSH, Base: "main", MergedAt: x.now.Add(-time.Hour)}} // before the executor existed
	x.f.files[7] = []string{"infra/a.tf"}
	x.poll(t)
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs)
	if len(runs.Items) != 0 {
		t.Fatal("merge from before the executor existed was planned")
	}
}

func TestExecutorPausedAndInvalid(t *testing.T) {
	x := newExecEnv(t, executor(func(e *v1.Executor) { e.Spec.Paused = true }))
	x.f.open = []pullRequest{{Number: 7, Head: headSHA, Base: "main"}}
	x.f.files[7] = []string{"infra/a.tf"}
	x.poll(t)
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs)
	if len(runs.Items) != 0 {
		t.Fatal("paused executor started work")
	}
	bad := map[string]func(*v1.Executor){
		"tag image":    func(e *v1.Executor) { e.Spec.Image = "hashicorp/terraform:1.9" },
		"other secret": func(e *v1.Executor) { e.Spec.ApplyCredentials.SecretName = "agent-alice" },
		"shared sa":    func(e *v1.Executor) { e.Spec.ApplyCredentials.ServiceAccountName = "exec-tf-prod-plan" },
		"forge secret": func(e *v1.Executor) { e.Spec.ForgeSecret = "agentdev-api-forge" },
		"path escape":  func(e *v1.Executor) { e.Spec.Path = "../etc" },
	}
	for n, m := range bad {
		if validExecutor(executor(m)) == "" {
			t.Errorf("%s: accepted", n)
		}
	}
	if msg := validExecutor(executor(nil)); msg != "" {
		t.Fatal(msg)
	}
}

func TestTouches(t *testing.T) {
	if !touches([]string{"infra/a.tf"}, "infra") || touches([]string{"infrastructure/a"}, "infra") || !touches(nil, "") {
		t.Fatal("touches")
	}
	many := make([]string, 100)
	if !touches(many, "infra") {
		t.Fatal("a truncated file list must count as touching")
	}
}

func TestPlanHelper(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "plan.out")
	if err := os.WriteFile(f, []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	code, ok := planHelper([]string{"hash-plan", f}, &b)
	h := parsePlanHash(b.String())
	if !ok || code != 0 || h != "64879f7d6b960a01909762d911a32d4582c20010c5641ee90278b644a9e3b525" {
		t.Fatalf("hash-plan %d %q", code, b.String())
	}
	b.Reset()
	if code, _ := planHelper([]string{"verify-plan", f, h}, &b); code != 0 {
		t.Fatalf("verify ok: %s", b.String())
	}
	if code, _ := planHelper([]string{"verify-plan", f, strings.Repeat("0", 64)}, &b); code != 1 {
		t.Fatal("verify must fail on another hash")
	}
	if code, _ := planHelper([]string{"verify-plan", f, "$(rm -rf /)"}, &b); code != 2 {
		t.Fatal("verify must refuse a malformed hash")
	}
	if code, _ := planHelper([]string{"hash-plan", dir}, &b); code != 1 {
		t.Fatal("hash-plan of a directory")
	}
	if _, ok := planHelper([]string{"--zap-devel"}, &b); ok {
		t.Fatal("operator flags are not helper calls")
	}
}

func TestApprovalHashMatchesSidecar(t *testing.T) {
	// same definition as images/forge approvalHash (#107): each field followed by NUL
	got := approvalHash("a", "b")
	if got != "8fb20ef63ced4145fc2e983ffe597d1dcff39154c3bf21f0fa9dde6a0c50fdc9" { // printf 'a\0b\0' | sha256sum
		t.Fatal(got)
	}
	if approvalHash("ab", "") == approvalHash("a", "b") {
		t.Fatal("field boundaries must matter")
	}
}
