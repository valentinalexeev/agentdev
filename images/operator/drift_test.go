package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #205 decision 4: the drift check plans the head of the protected branch with the check certificate, never keeps a
// plan or asks for an approval, opens one issue on drift and closes it when clean; a failure is never "clean".
func TestExecutorDrift(t *testing.T) {
	x, _ := sshExecEnv(t, readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t)))
	x.update(t, func(e *v1.Executor) {
		e.Spec.Drift = &v1.ExecutorDrift{ChangedPattern: `changed=[1-9]`, IntervalHours: 24}
	})
	x.f.head = mergeSH
	// waits for the proxy's SSH rule, like snapshots
	x.poll(t)
	var e v1.Executor
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	name := driftRunName(&e, x.now)
	if r := runOrNil(x, name); r != nil {
		t.Fatal("drift started before the proxy had the SSH rule")
	}
	sshEgressLoaded(t, x, x.now.Add(-time.Hour))
	x.poll(t)
	x.poll(t)
	r := x.run(t, name)
	if r.Spec.Stage != "drift" || r.Spec.Action != "plan" || r.Spec.Commit != mergeSH || r.Spec.PR != 0 || r.Status.Phase != "Running" {
		t.Fatalf("drift run %+v %+v", r.Spec, r.Status)
	}
	ps := x.job(t, name).Spec.Template.Spec
	if len(ps.InitContainers) < 1 || ps.InitContainers[0].Name != "checkout" || envVal(ps.InitContainers[0], "REF") != "refs/heads/main" {
		t.Fatalf("drift must check out the protected branch: %+v", ps.InitContainers)
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		if c.Name == "hash" || c.Name == "verify" {
			t.Fatalf("drift run has a %s container", c.Name)
		}
	}
	if ps.ServiceAccountName != "exec-tf-prod-plan" {
		t.Fatalf("drift must use the read-only identity, got %q", ps.ServiceAccountName)
	}
	for _, v := range ps.Volumes {
		if v.Name == "plan" && v.PersistentVolumeClaim != nil {
			t.Fatal("drift run has a plan volume")
		}
	}
	cert := parseCert(t, runSecret(t, x, name).Data["id_ed25519-cert.pub"])
	if strings.Join(cert.ValidPrincipals, ",") != "agentdev-tf-prod-check" {
		t.Fatalf("drift principals %v", cert.ValidPrincipals)
	}

	// drift: one issue, the output published redacted
	x.logs["tool"] = "web1 : ok=3 changed=1 unreachable=0\npassword: hunter2\n"
	x.finishJob(t, name, true)
	x.poll(t)
	if r := x.run(t, name); r.Status.Phase != "Succeeded" || r.Status.Message != "drift check: drift" {
		t.Fatalf("drift result %+v", r.Status)
	}
	if len(x.f.issues) != 1 || !strings.Contains(x.f.issues[0], "Drift: tf-prod (prod) differs from main") {
		t.Fatalf("issues %v", x.f.issues)
	}
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	if e.Status.DriftIssue != 101 {
		t.Fatalf("drift issue %d", e.Status.DriftIssue)
	}
	var cm corev1.ConfigMap
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: driftConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if got := cm.Data["tf-prod"]; strings.Contains(got, "hunter2") || !strings.Contains(got, "changed=1") || !strings.Contains(got, ": drift.") {
		t.Fatalf("published drift:\n%s", got)
	}
	if len(x.f.comments) != 0 {
		t.Fatalf("drift commented on a PR: %v", x.f.comments)
	}

	// next slot, still drift: a comment on the same issue, no new issue
	x.now = x.now.Add(24 * time.Hour)
	x.poll(t)
	name2 := driftRunName(&e, x.now)
	x.poll(t)
	x.finishJob(t, name2, true)
	x.poll(t)
	if len(x.f.issues) != 1 || len(x.f.comments) != 1 {
		t.Fatalf("second drift: issues %d comments %d", len(x.f.issues), len(x.f.comments))
	}

	// failed run: an error comment, never "clean", the issue stays open
	x.now = x.now.Add(24 * time.Hour)
	x.poll(t)
	name3 := driftRunName(&e, x.now)
	x.poll(t)
	x.logs["tool"] = "UNREACHABLE"
	x.finishJob(t, name3, false)
	x.poll(t)
	if x.f.closed[101] || x.run(t, name3).Status.Message != "drift check: error" {
		t.Fatalf("failed drift run closed the issue or was not an error: %+v", x.run(t, name3).Status)
	}

	// clean: the issue is closed
	x.now = x.now.Add(24 * time.Hour)
	x.poll(t)
	name4 := driftRunName(&e, x.now)
	x.poll(t)
	x.logs["tool"] = "web1 : ok=3 changed=0 unreachable=0\n"
	x.finishJob(t, name4, true)
	x.poll(t)
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	if !x.f.closed[101] || e.Status.DriftIssue != 0 {
		t.Fatalf("clean drift did not close the issue (closed %v, status %d)", x.f.closed, e.Status.DriftIssue)
	}
	if x.applies(t) != 0 {
		t.Fatal("a drift run led to an apply")
	}
	for _, a := range x.approvals(t) {
		t.Fatalf("a drift run requested an approval: %s", a)
	}
}

func TestDriftWithoutSSH(t *testing.T) {
	x := newExecEnv(t, executor(func(e *v1.Executor) {
		e.Spec.Drift = &v1.ExecutorDrift{ChangedPattern: `Plan: [0-9]+ to`}
	}))
	x.poll(t)
	var e v1.Executor
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	name := driftRunName(&e, x.now)
	x.poll(t)
	tool := x.job(t, name).Spec.Template.Spec.Containers[0]
	if tool.Name != "tool" || strings.Join(tool.Command, " ") != "terraform plan -out=$(PLAN_FILE)" {
		t.Fatalf("drift tool %+v", tool)
	}
	if runSecret(t, x, name) != nil {
		t.Fatal("drift of an executor without ssh got an SSH Secret")
	}
}

func TestDriftInvalidPattern(t *testing.T) {
	e := executor(func(e *v1.Executor) { e.Spec.Drift = &v1.ExecutorDrift{ChangedPattern: `(`} })
	if validExecutor(e) == "" {
		t.Fatal("an invalid drift pattern was accepted")
	}
	if v, _ := driftVerdict(e, true, "anything"); v != "error" {
		t.Fatalf("invalid pattern verdict %s", v)
	}
}

func runOrNil(x *execEnv, name string) *v1.ExecutorRun {
	var r v1.ExecutorRun
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: name}, &r); err != nil {
		return nil
	}
	return &r
}

func (x *execEnv) approvals(t *testing.T) []string {
	var l v1.ApprovalList
	if err := x.c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range l.Items {
		out = append(out, a.Name+" "+a.Spec.Tool)
	}
	return out
}
