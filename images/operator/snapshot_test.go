package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #205 decision 4: a scheduled snapshot runs the fixed command with the check certificate, no checkout and no PR,
// once per interval slot, and publishes redacted output for the agents.
func TestExecutorSnapshot(t *testing.T) {
	x, _ := sshExecEnv(t, readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t)))
	var e v1.Executor
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	e.Spec.Snapshot = &v1.ExecutorSnapshot{ExecutorCommand: v1.ExecutorCommand{Command: []string{"ansible", "all", "-i", "$(AGENTDEV_SSH_INVENTORY)", "-m", "setup"}},
		IntervalHours: 6}
	if err := x.c.Update(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	sshEgressLoaded(t, x, x.now.Add(-time.Hour))
	x.poll(t)
	name := snapshotRunName(&e, x.now)
	x.poll(t)
	r := x.run(t, name)
	if r.Spec.Stage != "snapshot" || r.Spec.Commit != "" || r.Spec.PR != 0 || r.Status.Phase != "Running" {
		t.Fatalf("snapshot run %+v %+v", r.Spec, r.Status)
	}
	ps := x.job(t, name).Spec.Template.Spec
	for _, c := range ps.InitContainers {
		if c.Name == "checkout" {
			t.Fatal("a snapshot checks out repository code")
		}
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		if c.Image == "" {
			t.Fatalf("container %s has no image", c.Name)
		}
	}
	tool := ps.Containers[0]
	if tool.Name != "tool" || strings.Join(tool.Command, " ") != "ansible all -i $(AGENTDEV_SSH_INVENTORY) -m setup" ||
		envVal(tool, "AGENTDEV_SSH_CONFIG") != "/ssh/config" {
		t.Fatalf("snapshot tool %+v", tool)
	}
	if ps.ServiceAccountName != "exec-tf-prod-plan" {
		t.Fatalf("snapshot must use the read-only identity, got %q", ps.ServiceAccountName)
	}
	sec := runSecret(t, x, name)
	cert := parseCert(t, sec.Data["id_ed25519-cert.pub"])
	if strings.Join(cert.ValidPrincipals, ",") != "agentdev-tf-prod-check" || cert.KeyId != "snapshot/"+name ||
		!strings.Contains(string(sec.Data["config"]), "User agentdev-check\n") {
		t.Fatalf("snapshot cert %v %q", cert.ValidPrincipals, cert.KeyId)
	}
	// result: published, redacted, no forge comment
	comments := len(x.f.comments)
	x.logs["tool"] = "web1 | SUCCESS => {\"ansible_facts\": {\"mem\": 2048}, \"db_password\": \"hunter2\"}\n" +
		"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\nAuthorization: Bearer abcdefghijklmnopqrstuvwxyz\n" +
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"
	x.finishJob(t, name, true)
	x.poll(t)
	if r := x.run(t, name); r.Status.Phase != "Succeeded" {
		t.Fatalf("snapshot not finished: %+v", r.Status)
	}
	var cm corev1.ConfigMap
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: snapshotConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	got := cm.Data["tf-prod"]
	for _, bad := range []string{"hunter2", "AAAA", "abcdefghijklmnopqrstuvwxyz\n", "ghp_abc"} {
		if strings.Contains(got, bad) {
			t.Errorf("snapshot not redacted (%q):\n%s", bad, got)
		}
	}
	if !strings.Contains(got, `"mem": 2048`) || !strings.Contains(got, "Untrusted data") {
		t.Fatalf("snapshot body:\n%s", got)
	}
	for _, ev := range x.events(t) {
		if !strings.HasPrefix(ev, "executor.snapshot/low") {
			t.Errorf("snapshot event %s", ev)
		}
	}
	if len(x.f.comments) != comments {
		t.Fatal("a snapshot commented on the forge")
	}
	// same slot: no second run; next slot: a new one
	x.poll(t)
	var runs v1.ExecutorRunList
	_ = x.c.List(context.Background(), &runs)
	n := 0
	for _, r := range runs.Items {
		if r.Spec.Stage == "snapshot" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d snapshot runs in one slot", n)
	}
	x.now = x.now.Add(6 * time.Hour)
	x.poll(t)
	x.run(t, snapshotRunName(&e, x.now))
}

// sshEgressLoaded sets the SSHEgress condition as syncEgress does once the proxy has the SSH rule.
func sshEgressLoaded(t *testing.T, x *execEnv, since time.Time) {
	t.Helper()
	var e v1.Executor
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	e.Status.Conditions = append(e.Status.Conditions, metav1.Condition{Type: "SSHEgress", Status: metav1.ConditionTrue,
		Reason: "RulesLoaded", LastTransitionTime: metav1.NewTime(since)})
	if err := x.c.Status().Update(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
}

// a snapshot waits until the proxy had the executor's SSH rule for a minute (no race with the reload)
func TestSnapshotWaitsForProxy(t *testing.T) {
	x, _ := sshExecEnv(t, readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t)))
	var e v1.Executor
	_ = x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
	e.Spec.Snapshot = &v1.ExecutorSnapshot{ExecutorCommand: v1.ExecutorCommand{Command: []string{"true"}}}
	_ = x.c.Update(context.Background(), &e)
	count := func() int {
		var runs v1.ExecutorRunList
		_ = x.c.List(context.Background(), &runs)
		return len(runs.Items)
	}
	x.poll(t)
	if count() != 0 {
		t.Fatal("snapshot without a loaded SSH rule")
	}
	sshEgressLoaded(t, x, x.now.Add(-10*time.Second))
	x.poll(t)
	if count() != 0 {
		t.Fatal("snapshot 10 s after the rule changed")
	}
	x.now = x.now.Add(time.Minute)
	x.poll(t)
	if count() != 1 {
		t.Fatal("no snapshot a minute after the rule was loaded")
	}
}

// syncEgress sets SSHEgress False when the SSH rules change and True on the next unchanged pass
func TestSSHEgressCondition(t *testing.T) {
	x, _ := sshExecEnv(t, readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t)))
	ctx := context.Background()
	cond := func() string {
		var e v1.Executor
		_ = x.c.Get(ctx, types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &e)
		if c := meta.FindStatusCondition(e.Status.Conditions, "SSHEgress"); c != nil {
			return string(c.Status)
		}
		return ""
	}
	sync := func() {
		var list v1.ExecutorList
		_ = x.c.List(ctx, &list)
		if err := x.p.syncEgress(ctx, list.Items, envOwners(list.Items)); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	if cond() != "False" {
		t.Fatalf("first pass (rules written): %q", cond())
	}
	sync()
	if cond() != "True" {
		t.Fatalf("unchanged pass: %q", cond())
	}
}

func TestRedactSnapshotClips(t *testing.T) {
	if s := redactSnapshot(strings.Repeat("x", snapshotMaxBytes+10)); len(s) > snapshotMaxBytes+20 || !strings.HasSuffix(s, "[clipped]") {
		t.Fatalf("not clipped: %d", len(s))
	}
}
