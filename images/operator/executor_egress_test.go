package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const execProxy = "http://executor-egress-proxy.agentdev-executors.svc.cluster.local:3128"

func egressEnvOf(t *testing.T, c corev1.Container) map[string]corev1.EnvVar {
	t.Helper()
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

// #135: with the executor proxy on, checkout and tool go out through it with the executor's own credential (from
// the operator's Secret, never a literal value); in-cluster names and the metadata server go direct.
func TestExecutorJobUsesEgressProxy(t *testing.T) {
	e := executor(nil)
	x := newExecEnv(t, e)
	x.p.C.EgressProxy = execProxy
	r := &v1.ExecutorRun{ObjectMeta: metav1.ObjectMeta{Name: "tf-prod-pr-1", Namespace: execNS},
		Spec: v1.ExecutorRunSpec{Executor: e.Name, Stage: "pr", Action: "plan", PR: 1, Commit: headSHA}}
	job := x.p.renderJob(e, r)
	pod := job.Spec.Template
	if pod.Labels["app"] != "agentdev-executor" {
		t.Fatalf("executor-egress selects app=agentdev-executor; pod labels %v", pod.Labels)
	}
	var seen int
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		env := egressEnvOf(t, c)
		if c.Name != "checkout" && c.Name != "tool" {
			if _, ok := env["HTTPS_PROXY"]; ok {
				t.Errorf("%s: helper containers need no egress", c.Name)
			}
			continue
		}
		seen++
		k := env["EXEC_EGRESS_KEY"]
		if k.ValueFrom == nil || k.ValueFrom.SecretKeyRef == nil || k.ValueFrom.SecretKeyRef.Name != execEgressSecret ||
			k.ValueFrom.SecretKeyRef.Key != "key-tf-prod" || k.Value != "" {
			t.Fatalf("%s: EXEC_EGRESS_KEY %+v", c.Name, k)
		}
		want := "http://tf-prod:$(EXEC_EGRESS_KEY)@executor-egress-proxy.agentdev-executors.svc.cluster.local:3128"
		for _, n := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			if env[n].Value != want {
				t.Errorf("%s: %s=%q", c.Name, n, env[n].Value)
			}
		}
		// the kubelet expands $(EXEC_EGRESS_KEY) only when it is defined earlier in the list
		idx := map[string]int{}
		for i, ev := range c.Env {
			idx[ev.Name] = i
		}
		if idx["EXEC_EGRESS_KEY"] > idx["HTTPS_PROXY"] {
			t.Errorf("%s: EXEC_EGRESS_KEY after HTTPS_PROXY", c.Name)
		}
		for _, h := range []string{".svc.cluster.local", "169.254.169.254", "metadata.google.internal"} {
			if !strings.Contains(env["NO_PROXY"].Value, h) || env["no_proxy"].Value != env["NO_PROXY"].Value {
				t.Errorf("%s: NO_PROXY lacks %s", c.Name, h)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("checkout and tool expected, saw %d", seen)
	}
	// off: no proxy env at all (clusters without the executor proxy)
	x.p.C.EgressProxy = ""
	for _, c := range x.p.renderJob(e, r).Spec.Template.Spec.InitContainers {
		if _, ok := egressEnvOf(t, c)["HTTPS_PROXY"]; ok {
			t.Fatal("proxy env without EXECUTOR_EGRESS_PROXY")
		}
	}
}

// #135: the allow-list of one executor: defaults, the forge's git host (not an in-cluster one) and spec.egressHosts;
// anything that is not a DNS name (Squid syntax, spaces, new lines, IP addresses) never reaches the proxy.
func TestExecutorEgressHosts(t *testing.T) {
	e := executor(func(e *v1.Executor) {
		e.Spec.Forge = "github"
		e.Spec.EgressHosts = []string{".googleapis.com", "registry.terraform.io", "evil.com\nhttp_access allow all",
			"all", "10.0.0.1", "1.2.3.4", "a b.com", "-x.com", ".Example.ORG"}
	})
	x := newExecEnv(t, e)
	x.p.C.EgressHosts = []string{"registry.terraform.io", "releases.hashicorp.com"}
	got := strings.Join(x.p.egressHosts(e), " ")
	want := ".example.org .googleapis.com github.com registry.terraform.io releases.hashicorp.com"
	if got != want {
		t.Fatalf("hosts %q, want %q", got, want)
	}
	gitea := executor(nil) // in-cluster forge: direct, not through the proxy
	if h := x.p.egressHosts(gitea); strings.Join(h, " ") != "registry.terraform.io releases.hashicorp.com" {
		t.Fatalf("gitea hosts %v", h)
	}
}

// #135: the operator keeps one key per runnable executor and the Squid rules that bind the key to that executor's
// hosts only. Keys survive passes; an invalid or duplicate executor gets none; a removed one loses its key.
func TestExecutorEgressSecret(t *testing.T) {
	a := executor(func(e *v1.Executor) { e.Spec.EgressHosts = []string{".googleapis.com"} })
	b := executor(func(e *v1.Executor) {
		e.Name, e.UID, e.Spec.Environment, e.Spec.ForgeSecret = "tf-dev", "uid-dev", "dev", "exec-tf-dev-forge"
		e.Spec.PlanCredentials, e.Spec.ApplyCredentials = v1.ExecutorCredentials{}, v1.ExecutorCredentials{}
	})
	dup := executor(func(e *v1.Executor) { // same environment as a, newer: invalid
		e.Name, e.UID, e.Spec.ForgeSecret = "tf-prod2", "uid-2", "exec-tf-prod2-forge"
		e.Spec.PlanCredentials, e.Spec.ApplyCredentials = v1.ExecutorCredentials{}, v1.ExecutorCredentials{}
		e.CreationTimestamp = metav1.NewTime(e.CreationTimestamp.AddDate(0, 0, 1))
		e.Spec.EgressHosts = []string{"exfil.example.com"}
	})
	x := newExecEnv(t, a, b, dup)
	x.p.C.EgressProxy = execProxy
	x.p.C.EgressHosts = []string{"registry.terraform.io"}
	ctx := context.Background()
	sync := func() *corev1.Secret {
		t.Helper()
		var list v1.ExecutorList
		if err := x.c.List(ctx, &list); err != nil {
			t.Fatal(err)
		}
		if err := x.p.syncEgress(ctx, list.Items, envOwners(list.Items)); err != nil {
			t.Fatal(err)
		}
		var s corev1.Secret
		if err := x.c.Get(ctx, types.NamespacedName{Namespace: execNS, Name: execEgressSecret}, &s); err != nil {
			t.Fatal(err)
		}
		return &s
	}
	s := sync()
	ka, kb := string(s.Data["key-tf-prod"]), string(s.Data["key-tf-dev"])
	if !hexKeyRE.MatchString(ka) || !hexKeyRE.MatchString(kb) || ka == kb {
		t.Fatalf("keys %q %q", ka, kb)
	}
	if _, ok := s.Data["key-tf-prod2"]; ok {
		t.Fatal("a duplicate executor got a proxy key")
	}
	acl := string(s.Data[execEgressACL])
	credA := regexp.QuoteMeta(base64.StdEncoding.EncodeToString([]byte("tf-prod:" + ka)))
	for _, want := range []string{
		"acl exec_tf_prod_client req_header Proxy-Authorization ^Basic." + credA + "$\n",
		"acl exec_tf_prod_hosts dstdomain -n .googleapis.com registry.terraform.io\n",
		"http_access allow exec_tf_prod_client exec_tf_prod_hosts\n",
		"acl exec_tf_dev_hosts dstdomain -n registry.terraform.io\n",
	} {
		if !strings.Contains(acl, want) {
			t.Errorf("acl.conf lacks %q:\n%s", want, acl)
		}
	}
	if strings.Contains(acl, "exfil") || strings.Contains(acl, "tf_prod2") {
		t.Fatalf("duplicate executor's hosts in acl.conf:\n%s", acl)
	}
	// every line is one of the three generated forms: nothing else can reach squid.conf
	lineRE := regexp.MustCompile(`^(#.*|acl exec_[a-z0-9_]+_client req_header Proxy-Authorization \^Basic\.[A-Za-z0-9+/=\\]+\$|acl exec_[a-z0-9_]+_hosts dstdomain -n [a-z0-9. -]+|http_access allow exec_[a-z0-9_]+_client exec_[a-z0-9_]+_hosts)$`)
	for _, l := range strings.Split(strings.TrimSpace(acl), "\n") {
		if !lineRE.MatchString(l) {
			t.Errorf("unexpected acl.conf line %q", l)
		}
	}
	// stable across passes
	if s2 := sync(); string(s2.Data["key-tf-prod"]) != ka || s2.ResourceVersion != s.ResourceVersion {
		t.Fatal("a second pass changed the Secret")
	}
	// an admin adds a host: same key, new rule
	var cur v1.Executor
	_ = x.c.Get(ctx, types.NamespacedName{Namespace: execNS, Name: "tf-prod"}, &cur)
	cur.Spec.EgressHosts = append(cur.Spec.EgressHosts, "storage.googleapis.com")
	if err := x.c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	s3 := sync()
	if string(s3.Data["key-tf-prod"]) != ka || !strings.Contains(string(s3.Data[execEgressACL]), "storage.googleapis.com") {
		t.Fatal("added host not in acl.conf, or key rotated")
	}
	// removed executor: key and rules gone
	if err := x.c.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	s4 := sync()
	if _, ok := s4.Data["key-tf-dev"]; ok || strings.Contains(string(s4.Data[execEgressACL]), "tf_dev") {
		t.Fatal("removed executor keeps its proxy access")
	}
}

// #135: a refusal of the executor proxy becomes a SecurityEvent of the executor whose pod made the request; a line
// read again is the same event; an unknown client is dropped.
func TestExecutorEgressRefusalEvent(t *testing.T) {
	e := executor(nil)
	x := newExecEnv(t, e)
	px := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "executor-egress-proxy-a", Namespace: execNS, UID: "px"}}
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "tf-prod-pr-1-x", Namespace: execNS,
		Labels: map[string]string{"app": "agentdev-executor", "agentdev/executor": "tf-prod", "job-name": "tf-prod-pr-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"}}}
	log := strings.Join([]string{
		"2026-11-01T10:00:00Z 2026/11/01 10:00:00| Starting Squid",
		line("2026-11-01T10:00:01Z", `{"rule":"egress-host","severity":"medium","client":"10.0.0.9","action":"CONNECT exfil.example:443"}`),
		line("2026-11-01T10:00:02Z", `{"rule":"egress-host","severity":"medium","client":"10.9.9.9","action":"CONNECT x:443"}`),
	}, "\n")
	byName := map[string]*v1.Executor{"tf-prod": e}
	for i := 0; i < 2; i++ {
		last := x.p.consumeEgress(context.Background(), strings.NewReader(log), px, pods, byName)
		if last.Format("15:04:05") != "10:00:02" {
			t.Fatalf("last %v", last)
		}
	}
	var evs v1.SecurityEventList
	if err := x.c.List(context.Background(), &evs); err != nil {
		t.Fatal(err)
	}
	if len(evs.Items) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs.Items))
	}
	ev := evs.Items[0]
	if ev.Namespace != ns || ev.Spec.Agent != "tf-prod" || ev.Spec.Source != "egress" || ev.Spec.Rule != "egress-host" ||
		ev.Spec.Severity != "medium" || ev.Spec.Action != "CONNECT exfil.example:443" || ev.Spec.Pod != "tf-prod-pr-1-x" ||
		ev.Labels["agentdev/source"] != "egress" {
		t.Fatalf("event %+v %v", ev.Spec, ev.Labels)
	}
}

// #135 review: PR code controls the refused requests, so a flood must not flood the event store. Repeats of a host
// in the window are one event; past the cap there is one rate-limited event and the rest are counted as dropped; a
// new window starts again; a second read of the same log adds nothing.
func TestExecutorEgressRefusalFlood(t *testing.T) {
	e := executor(nil)
	x := newExecEnv(t, e)
	x.p.C.EgressMaxEvents = 3
	px := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "executor-egress-proxy-a", Namespace: execNS, UID: "px"}}
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "tf-prod-pr-1-x", Namespace: execNS,
		Labels: map[string]string{"app": "agentdev-executor", "agentdev/executor": "tf-prod", "job-name": "tf-prod-pr-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"}}}
	var lines []string
	for i := 0; i < 1000; i++ { // 1000 refusals of 100 hosts within one minute
		ts := fmt.Sprintf("2026-11-01T10:00:%02d.%06dZ", i/20, i)
		lines = append(lines, line(ts, fmt.Sprintf(`{"rule":"egress-host","severity":"medium","client":"10.0.0.9","action":"CONNECT h%d.example:443"}`, i%100)))
	}
	lines = append(lines, line("2026-11-01T10:20:00Z", `{"rule":"egress-host","severity":"medium","client":"10.0.0.9","action":"GET http://late.example/x"}`))
	byName := map[string]*v1.Executor{"tf-prod": e}
	for i := 0; i < 2; i++ {
		x.p.consumeEgress(context.Background(), strings.NewReader(strings.Join(lines, "\n")), px, pods, byName)
	}
	var evs v1.SecurityEventList
	if err := x.c.List(context.Background(), &evs); err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	var actions []string
	for _, ev := range evs.Items {
		rules[ev.Spec.Rule]++
		actions = append(actions, ev.Spec.Action)
	}
	if len(evs.Items) != 5 || rules["egress-host"] != 4 || rules["rate-limited"] != 1 {
		t.Fatalf("want 3 hosts + 1 rate-limited + 1 in the next window, got %v %v", rules, actions)
	}
	for _, ev := range evs.Items {
		if ev.Spec.Rule == "rate-limited" && (ev.Spec.Severity != "medium" || ev.Spec.Agent != "tf-prod") {
			t.Fatalf("rate-limited event %+v", ev.Spec)
		}
	}
	if got := x.p.EgressDropped.Load(); got < 1990 {
		t.Fatalf("dropped %d, want the repeats and the lines over the cap counted", got)
	}
	if h := egressHostOf("GET http://Late.example:8080/x"); h != "late.example" {
		t.Fatalf("host %q", h)
	}
}

// #135: the executor egress manifest renders; executor-egress is default deny for every executor pod the operator
// renders (DNS, the proxy, the forge, the metadata server only); squid.conf has the guardrails and includes the
// operator's rules between the deny rules and the final deny.
func TestExecutorEgressManifest(t *testing.T) {
	docs := renderTmpl(t, "../../manifests/executor-egress.yaml.tmpl", map[string]string{"EXEC_NS": execNS,
		"FORGE_NS": "forge", "EGRESS_PROXY_IMAGE": "img"})
	docs = append(docs, renderTmpl(t, "../../manifests/executor-egress-web.yaml.tmpl", map[string]string{"EXEC_NS": execNS})...)
	var conf string
	var dep appsv1.Deployment
	pols := map[string]networkingv1.NetworkPolicy{}
	for _, d := range docs {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(d), &meta); err != nil {
			t.Fatalf("yaml: %v\n%s", err, d)
		}
		switch meta.Kind {
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := yaml.UnmarshalStrict([]byte(d), &cm); err != nil {
				t.Fatal(err)
			}
			conf = cm.Data["squid.conf"]
		case "NetworkPolicy":
			var p networkingv1.NetworkPolicy
			if err := yaml.UnmarshalStrict([]byte(d), &p); err != nil {
				t.Fatal(err)
			}
			if p.Namespace != execNS {
				t.Errorf("%s in %s", p.Name, p.Namespace)
			}
			pols[p.Name] = p
		case "Deployment":
			if err := yaml.UnmarshalStrict([]byte(d), &dep); err != nil {
				t.Fatal(err)
			}
		case "Service":
		default:
			t.Errorf("unexpected kind %s", meta.Kind)
		}
	}
	deny := strings.Index(conf, "http_access deny private_dst\n")
	inc := strings.Index(conf, "include /tmp/acl.conf\n")
	all := strings.Index(conf, "http_access deny all\n")
	if deny < 0 || inc < deny || all < inc {
		t.Fatalf("squid.conf order: deny private_dst %d, include %d, deny all %d", deny, inc, all)
	}
	if strings.Contains(conf, "http_access allow") {
		t.Error("squid.conf must allow nothing itself (only the operator's per-executor rules)")
	}
	for _, want := range []string{"169.254.0.0/16", "::ffff:0:0/96", "64:ff9b::/96", "AGENTDEV_SECURITY_EVENT"} {
		if !strings.Contains(conf, want) {
			t.Errorf("squid.conf lacks %q", want)
		}
	}
	if regexp.MustCompile(`logformat agentdev .*%[#>]*ru`).MatchString(conf) {
		t.Error("refusal log contains the URL")
	}
	// the proxy reads the operator's Secret (optional: it starts and refuses everything before the first Executor)
	var acl *corev1.SecretVolumeSource
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == execEgressSecret {
			acl = v.Secret
		}
	}
	if acl == nil || acl.Optional == nil || !*acl.Optional || len(acl.Items) != 1 || acl.Items[0].Key != execEgressACL {
		t.Fatalf("proxy does not mount %s/%s: %+v", execEgressSecret, execEgressACL, acl)
	}
	if dep.Spec.Template.Labels["app"] != execEgressApp || dep.Spec.Template.Spec.Containers[0].Name != "squid" {
		t.Fatal("collectEgress looks for app=executor-egress-proxy, container squid")
	}

	ee, ok := pols["executor-egress"]
	if !ok || len(ee.Spec.PolicyTypes) != 1 || ee.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatal("executor-egress missing or not an egress policy")
	}
	for _, r := range ee.Spec.Egress {
		if len(r.To) == 0 || len(r.Ports) == 0 {
			t.Errorf("executor-egress rule without destination or ports: %+v", r)
		}
		for _, p := range r.To {
			if p.IPBlock != nil && p.IPBlock.CIDR != "169.254.169.254/32" && p.IPBlock.CIDR != "169.254.169.252/32" {
				t.Errorf("executor-egress allows IP block %v", p.IPBlock)
			}
		}
	}
	for _, n := range []string{"executor-egress-proxy", "executor-egress-proxy-dns", "executor-egress-proxy-web"} {
		if _, ok := pols[n]; !ok {
			t.Errorf("policy %s missing", n)
		}
	}
	// executor-egress selects the pods of every Job the operator renders
	e := executor(nil)
	x := newExecEnv(t, e)
	for _, stage := range []string{"pr", "merge", "apply"} {
		act := "plan"
		if stage == "apply" {
			act = "apply"
		}
		job := x.p.renderJob(e, &v1.ExecutorRun{ObjectMeta: metav1.ObjectMeta{Name: "r"}, Spec: v1.ExecutorRunSpec{Stage: stage, Action: act, PR: 1, Commit: headSHA}})
		sel, err := metav1.LabelSelectorAsSelector(&ee.Spec.PodSelector)
		if err != nil || !sel.Matches(labels.Set(job.Spec.Template.Labels)) {
			t.Fatalf("executor-egress does not select the %s Job pod (%v)", stage, job.Spec.Template.Labels)
		}
		px, _ := metav1.LabelSelectorAsSelector(pols["executor-egress-proxy"].Spec.Ingress[0].From[0].PodSelector)
		if !px.Matches(labels.Set(job.Spec.Template.Labels)) {
			t.Fatalf("the %s Job pod may not reach the proxy", stage)
		}
	}
}
