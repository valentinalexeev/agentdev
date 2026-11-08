package main

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func ansibleExecutor() *v1.Executor {
	return executor(func(e *v1.Executor) {
		e.Spec.SSH = true
		e.Spec.Plan = v1.ExecutorCommand{Command: []string{"sh", "-c", "ansible-playbook -i $(AGENTDEV_SSH_INVENTORY) site.yml --check --diff > $(PLAN_FILE)"}}
		e.Spec.Apply = v1.ExecutorCommand{Command: []string{"ansible-playbook", "-i", "$(AGENTDEV_SSH_INVENTORY)", "site.yml"}}
		e.Spec.PRPlan = &v1.ExecutorCommand{Command: []string{"sh", "-c", "ansible-playbook --syntax-check site.yml > $(PLAN_FILE)"}}
	})
}

func readyTarget(name, executor, host string, hostCA ssh.PublicKey) *v1.SSHTarget {
	return &v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: execNS},
		Spec: v1.SSHTargetSpec{Host: host, Executor: executor, HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43)},
		Status: v1.SSHTargetStatus{Phase: "Ready", HostCAKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostCA))),
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ProbeSucceeded",
				LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))}}}}
}

func sshExecEnv(t *testing.T, targets ...*v1.SSHTarget) (*execEnv, *fakeStepCA) {
	f, _, ca := newFakeStepCA(t)
	var objs []client.Object
	for _, tg := range targets {
		if tg.Status.HostCAKey == "" {
			tg.Status.HostCAKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(f.hostCA.PublicKey())))
		}
		objs = append(objs, tg)
	}
	x := newExecEnv(t, ansibleExecutor(), objs...)
	x.p.SSHCA = func(context.Context) (*SSHCA, error) { return ca, nil }
	x.p.C.EgressProxy = "http://executor-egress-proxy.agentdev-executors.svc:3128"
	return x, f
}

func runSecret(t *testing.T, x *execEnv, run string) *corev1.Secret {
	var s corev1.Secret
	if err := x.c.Get(context.Background(), types.NamespacedName{Namespace: execNS, Name: run + "-ssh"}, &s); err != nil {
		return nil
	}
	return &s
}

// #204, ADR 0003 D5: a PR head never gets a server credential; the merge plan gets the check principal, the apply the
// apply principal, each with a fresh 15-minute certificate whose key ID names the run.
func TestExecutorSSHStages(t *testing.T) {
	x, f := sshExecEnv(t, &v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: "web1", Namespace: execNS},
		Spec:   v1.SSHTargetSpec{Host: "web1.example.com", Executor: "tf-prod", HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43)},
		Status: v1.SSHTargetStatus{Phase: "Ready"}})

	// PR head
	x.f.open = []pullRequest{{Number: 9, Head: headSHA, Base: "main", HeadRepo: "acme/platform"}}
	x.f.files[9] = []string{"infra/site.yml"}
	n := f.requests
	x.poll(t)
	x.poll(t)
	pr := "tf-prod-pr-" + headSHA[:12]
	ps := x.job(t, pr).Spec.Template.Spec
	if runSecret(t, x, pr) != nil || f.requests != n {
		t.Fatal("PR head got an SSH certificate")
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, "AGENTDEV_SSH") || e.Name == "ANSIBLE_SSH_ARGS" || strings.Contains(e.Value, "-ssh:") ||
				(e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.HasSuffix(e.ValueFrom.SecretKeyRef.Key, "-ssh")) {
				t.Fatalf("PR container %s has %s", c.Name, e.Name)
			}
		}
		for _, m := range c.VolumeMounts {
			if m.Name == "ssh" {
				t.Fatal("PR Job mounts ssh")
			}
		}
	}
	tool := ps.InitContainers[1]
	if tool.Name != "tool" || !strings.Contains(strings.Join(tool.Command, " "), "--syntax-check") {
		t.Fatalf("PR head must run spec.prPlan, got %v", tool.Command)
	}

	// merge plan: check principal, user agentdev-check
	an := mergeToApproval(t, x)
	merge := "tf-prod-merge-" + mergeSH[:12]
	sec := runSecret(t, x, merge)
	if sec == nil {
		t.Fatal("merge plan has no SSH Secret")
	}
	cert := parseCert(t, sec.Data["id_ed25519-cert.pub"])
	if strings.Join(cert.ValidPrincipals, ",") != "agentdev-tf-prod-check" || cert.KeyId != "executor/"+merge+"/"+mergeSH {
		t.Fatalf("merge cert principals %v key id %q", cert.ValidPrincipals, cert.KeyId)
	}
	if d := time.Duration(cert.ValidBefore-cert.ValidAfter) * time.Second; d > 17*time.Minute {
		t.Fatalf("certificate valid %s", d)
	}
	cfg := string(sec.Data["config"])
	for _, want := range []string{"User agentdev-check", "StrictHostKeyChecking yes", "connect-proxy %h %p", "Host web1.example.com",
		"HostKeyAlgorithms ssh-ed25519-cert-v01@openssh.com", "ForwardAgent no"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("ssh config lacks %q:\n%s", want, cfg)
		}
	}
	if !strings.HasPrefix(string(sec.Data["known_hosts"]), "@cert-authority web1.example.com ssh-ed25519 ") {
		t.Fatalf("known_hosts %q", sec.Data["known_hosts"])
	}
	if !strings.Contains(string(sec.Data["inventory.ini"]), "web1 ansible_host=web1.example.com ansible_port=22") {
		t.Fatalf("inventory %q", sec.Data["inventory.ini"])
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "ExecutorRun" || sec.OwnerReferences[0].Name != merge {
		t.Fatalf("SSH Secret owner %+v", sec.OwnerReferences)
	}
	ms := x.job(t, merge).Spec.Template.Spec
	if ms.InitContainers[0].Name != "checkout" || ms.InitContainers[1].Name != "ssh-helper" || ms.InitContainers[2].Name != "tool" {
		t.Fatalf("init order %v %v %v", ms.InitContainers[0].Name, ms.InitContainers[1].Name, ms.InitContainers[2].Name)
	}
	mounted := map[string][]string{}
	for _, c := range append(ms.InitContainers, ms.Containers...) {
		for _, m := range c.VolumeMounts {
			mounted[m.Name] = append(mounted[m.Name], c.Name)
		}
	}
	if strings.Join(mounted["ssh"], ",") != "tool,tool,tool" { // /ssh, /etc/passwd, /etc/group
		t.Fatalf("the SSH Secret must be mounted only in the tool container, got %v", mounted["ssh"])
	}
	if envVal(ms.InitContainers[2], "ANSIBLE_SSH_ARGS") != "-F /ssh/config" {
		t.Fatal("ANSIBLE_SSH_ARGS")
	}
	// the tool of the merge plan uses the SSH proxy credential; checkout keeps the plain one
	if p := envVal(ms.InitContainers[2], "HTTPS_PROXY"); !strings.Contains(p, "tf-prod-ssh:$(EXEC_EGRESS_KEY)@") {
		t.Fatalf("merge tool proxy %q", p)
	}
	if p := envVal(ms.InitContainers[0], "HTTPS_PROXY"); strings.Contains(p, "-ssh:") {
		t.Fatalf("checkout proxy %q", p)
	}

	// apply: apply principal, user agentdev
	x.decide(t, an, "allowed", nil)
	x.poll(t)
	x.poll(t)
	apply := "tf-prod-apply-" + mergeSH[:12]
	asec := runSecret(t, x, apply)
	if asec == nil {
		t.Fatal("apply has no SSH Secret")
	}
	ac := parseCert(t, asec.Data["id_ed25519-cert.pub"])
	if strings.Join(ac.ValidPrincipals, ",") != "agentdev-tf-prod-apply" || !strings.Contains(string(asec.Data["config"]), "User agentdev\n") {
		t.Fatalf("apply cert %v", ac.ValidPrincipals)
	}
	if string(asec.Data["id_ed25519"]) == string(sec.Data["id_ed25519"]) {
		t.Fatal("apply reuses the merge key")
	}
	as := x.job(t, apply).Spec.Template.Spec
	if as.Containers[0].Name != "tool" || envVal(as.Containers[0], "AGENTDEV_SSH_CONFIG") != "/ssh/config" {
		t.Fatal("apply tool container without SSH config")
	}
}

func parseCert(t *testing.T, b []byte) *ssh.Certificate {
	t.Helper()
	pk, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := pk.(*ssh.Certificate)
	if !ok {
		t.Fatal("not a certificate")
	}
	return c
}

// spec.ssh without a Ready target, or without a CA, fails the run closed (no Job); targets with different local users
// are refused; spec.ssh without prPlan is invalid.
func TestExecutorSSHFailsClosed(t *testing.T) {
	x, _ := sshExecEnv(t)
	mergeRun := func(x *execEnv) *v1.ExecutorRun {
		x.f.merged = []pullRequest{{Number: 7, Head: headSHA, Merge: mergeSH, Base: "main", MergedAt: x.now.Add(-time.Hour)}}
		x.f.files[7] = []string{"infra/site.yml"}
		x.poll(t)
		x.poll(t)
		return x.run(t, "tf-prod-merge-"+mergeSH[:12])
	}
	if r := mergeRun(x); r.Status.Phase != "Failed" || !strings.Contains(r.Status.Message, "no Ready SSHTarget") {
		t.Fatalf("no target: %+v", r.Status)
	}
	x2, _ := sshExecEnv(t, readyTarget("a", "tf-prod", "a.example.com", nil2key(t)), func() *v1.SSHTarget {
		tg := readyTarget("b", "tf-prod", "b.example.com", nil2key(t))
		tg.Spec.CheckUser = "deploy-check"
		return tg
	}())
	if r := mergeRun(x2); r.Status.Phase != "Failed" || !strings.Contains(r.Status.Message, "different local users") {
		t.Fatalf("mixed users: %+v", r.Status)
	}
	x3, _ := sshExecEnv(t, readyTarget("a", "tf-prod", "a.example.com", nil2key(t)))
	x3.p.SSHCA = nil
	if r := mergeRun(x3); r.Status.Phase != "Failed" || !strings.Contains(r.Status.Message, "SSH CA") {
		t.Fatalf("no CA: %+v", r.Status)
	}
	e := ansibleExecutor()
	e.Spec.PRPlan = nil
	if msg := validExecutor(e); !strings.Contains(msg, "prPlan") {
		t.Fatalf("ssh without prPlan: %q", msg)
	}
}

func base64Std(s string) string {
	return regexp.QuoteMeta(base64.StdEncoding.EncodeToString([]byte(s)))
}

func nil2key(t *testing.T) ssh.PublicKey {
	_, pub := testKey(t)
	return pub
}

// the executor egress proxy opens CONNECT to a Ready SSHTarget only with its executor's credential (#204)
func TestExecutorEgressSSHACL(t *testing.T) {
	e := *ansibleExecutor()
	other := *executor(func(o *v1.Executor) { o.Name = "tf-dev" }) // no spec.ssh
	keys := map[string]string{"tf-prod": strings.Repeat("a", 48)}  // SSH keys: only executors with spec.ssh have one
	hostCA := nil2key(t)
	targets := map[string][]v1.SSHTarget{
		"tf-prod": {*readyTarget("web1", "tf-prod", "web1.example.com", hostCA), *readyTarget("db", "tf-prod", "10.0.0.5", hostCA),
			*readyTarget("bad", "tf-prod", "x example.com\nhttp_access allow all", hostCA)},
		"tf-dev": {*readyTarget("dev1", "tf-dev", "dev1.example.com", hostCA)},
	}
	targets["tf-prod"][1].Spec.Port = 2222
	acl := egressSSHACL([]v1.Executor{e, other}, keys, targets)
	// sorted by target name: db (an address, port 2222) first, then web1
	if !strings.Contains(acl, "acl exec_tf_prod_ssh0_host dst 10.0.0.5\nacl exec_tf_prod_ssh0_port port 2222\n") {
		t.Errorf("address target:\n%s", acl)
	}
	if !strings.Contains(acl, "acl exec_tf_prod_ssh1_host dstdomain -n web1.example.com\nacl exec_tf_prod_ssh1_port port 22\n") {
		t.Errorf("name target:\n%s", acl)
	}
	if !strings.Contains(acl, "http_access allow CONNECT exec_tf_prod_sshclient exec_tf_prod_ssh1_host exec_tf_prod_ssh1_port") {
		t.Errorf("allow rule:\n%s", acl)
	}
	if strings.Contains(acl, "dev1") || strings.Contains(acl, "tf_dev") {
		t.Error("an executor without spec.ssh gets an SSH rule")
	}
	if strings.Contains(acl, "allow all") || strings.Contains(acl, "x example") {
		t.Error("an invalid host reached the ACL")
	}
	if strings.Count(acl, "http_access") != 2 {
		t.Errorf("rules:\n%s", acl)
	}
	// the SSH rule matches the <name>-ssh credential, never the plain one that PR Jobs have
	if !strings.Contains(acl, base64Std("tf-prod-ssh:"+keys["tf-prod"])) || strings.Contains(acl, base64Std("tf-prod:"+keys["tf-prod"])) {
		t.Errorf("SSH rule not bound to the SSH credential:\n%s", acl)
	}
}
