package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// renderTmpl does what envsubst does for manifests/*.tmpl (06-agents.sh) with the given values.
func renderTmpl(t *testing.T, path string, vals map[string]string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := os.Expand(string(b), func(k string) string { return vals[k] })
	if strings.Contains(s, "${") {
		t.Fatalf("%s: unrendered variable", path)
	}
	return strings.Split(s, "\n---\n")
}

// #65: the egress manifest renders, its squid.conf has the guardrails, and agent-egress selects every agent pod the
// operator renders (a label change in render.go must not silently open egress).
func TestEgressManifest(t *testing.T) {
	docs := renderTmpl(t, "../../manifests/egress.yaml.tmpl", map[string]string{"AGENTS_NS": ns, "PLATFORM_NS": "agentdev-platform",
		"FORGE_NS": "forge", "BOTBOARD_NS": "botboard", "EGRESS_HOSTS": "api.anthropic.com .github.com", "EGRESS_PROXY_IMAGE": "img"})
	var conf string
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
			pols[p.Name] = p
		case "Deployment":
			var dep appsv1.Deployment
			if err := yaml.UnmarshalStrict([]byte(d), &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, want := range []string{
		"acl allowed dstdomain -n api.anthropic.com .github.com",
		"http_access deny private_dst\n",
		"169.254.0.0/16", "::ffff:0:0/96", "64:ff9b::/96",
		"http_access deny all\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("squid.conf lacks %q", want)
		}
	}
	// deny rules come before the allow rule (Squid uses the first match)
	if strings.Index(conf, "http_access allow allowed") < strings.Index(conf, "http_access deny private_dst") {
		t.Error("allow before deny private_dst")
	}
	// the refusal line must not log the URL path or query (credentials): %ru / %>ru are not allowed
	if regexp.MustCompile(`logformat agentdev .*%[#>]*ru`).MatchString(conf) {
		t.Error("refusal log contains the URL")
	}
	ae, ok := pols["agent-egress"]
	if !ok || len(ae.Spec.PolicyTypes) != 1 || ae.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatal("agent-egress missing or not an egress policy")
	}
	for _, r := range ae.Spec.Egress { // no rule may allow every destination
		if len(r.To) == 0 {
			t.Error("agent-egress has a rule without a destination")
		}
		for _, p := range r.To {
			if p.IPBlock != nil {
				t.Errorf("agent-egress allows an IP block %v", p.IPBlock)
			}
		}
	}
	if _, ok := pols["egress-proxy-dns"]; !ok {
		t.Error("the proxy needs its DNS policy (an FQDN policy isolates it)")
	}
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	sel, err := metav1.LabelSelectorAsSelector(&ae.Spec.PodSelector)
	if err != nil || !sel.Matches(labels.Set(s.Spec.Template.Labels)) {
		t.Fatalf("agent-egress does not select the agent pod (%v)", s.Spec.Template.Labels)
	}
}

// #118: a chat session (manifests/chat.yaml.tmpl) mounts no Claude Secret, calls claude-proxy with its agent's proxy
// key, uses the egress proxy, and agent-egress, egress-proxy and claude-proxy all select its pod.
func TestChatSessionUsesClaudeProxy(t *testing.T) {
	proxyURL := "http://claude-proxy." + ns + ".svc.cluster.local:8080"
	egress := "http://egress-proxy." + ns + ".svc.cluster.local:3128"
	docs := renderTmpl(t, "../../manifests/chat.yaml.tmpl", map[string]string{"SESSION": "chat-p-120000", "AGENTS_NS": ns,
		"PROJECT": "p", "CHAT_AGENT": "alice", "WORKER_IMAGE": "img", "IMAGE_PULL_POLICY": "Never", "FORGE_URL": "http://gitea",
		"CHAT_USER": "p-chat", "REPO": "o/p", "IDLE_SECONDS": "1800", "REMOTE_CONTROL": "false", "BOTBOARD_URL": "",
		"CHAT_URL": "", "CLAUDE_PROXY_URL": proxyURL, "EGRESS_PROXY": egress})
	if len(docs) != 1 {
		t.Fatalf("want one document, got %d", len(docs))
	}
	var job batchv1.Job
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	refs := []string{}
	for _, c := range append(pod.InitContainers, pod.Containers...) {
		for _, e := range c.EnvFrom {
			if e.SecretRef != nil {
				refs = append(refs, e.SecretRef.Name)
			}
		}
		env := map[string]corev1.EnvVar{}
		for _, e := range c.Env {
			env[e.Name] = e
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				refs = append(refs, e.ValueFrom.SecretKeyRef.Name)
			}
			if strings.Contains(e.Name, "ANTHROPIC_API_KEY") || strings.Contains(e.Name, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("credential variable %s in the session pod", e.Name)
			}
		}
		if env["ANTHROPIC_BASE_URL"].Value != proxyURL {
			t.Errorf("ANTHROPIC_BASE_URL = %q", env["ANTHROPIC_BASE_URL"].Value)
		}
		if k := env["ANTHROPIC_AUTH_TOKEN"].ValueFrom; k == nil || k.SecretKeyRef == nil || k.SecretKeyRef.Name != "agent-alice-proxy" {
			t.Errorf("ANTHROPIC_AUTH_TOKEN is not the proxy key of alice: %+v", env["ANTHROPIC_AUTH_TOKEN"])
		}
		if env["HTTPS_PROXY"].Value != egress || !strings.Contains(env["NO_PROXY"].Value, ".svc.cluster.local") {
			t.Errorf("HTTPS_PROXY=%q NO_PROXY=%q", env["HTTPS_PROXY"].Value, env["NO_PROXY"].Value)
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			refs = append(refs, v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil {
					refs = append(refs, s.Secret.Name)
				}
			}
		}
	}
	for _, r := range refs {
		if strings.HasPrefix(r, "claude-") {
			t.Errorf("session pod references Claude Secret %s", r)
		}
	}
	// the NetworkPolicies of egress.yaml.tmpl and claude-proxy.yaml.tmpl
	vals := map[string]string{"AGENTS_NS": ns, "PLATFORM_NS": "agentdev-platform", "FORGE_NS": "forge", "BOTBOARD_NS": "botboard",
		"EGRESS_HOSTS": ".github.com", "FORGE_EGRESS_HOSTS": "github.com", "EGRESS_PROXY_IMAGE": "img", "FORGE_PROXY_AUTH_SHA": "x",
		"CLAUDE_PROXY_IMAGE": "img", "IMAGE_PULL_POLICY": "Never", "CLAUDE_BUDGET_TOKENS": "0", "CLAUDE_SECRET_SOURCES": "[]"}
	docs = append(renderTmpl(t, "../../manifests/egress.yaml.tmpl", vals), renderTmpl(t, "../../manifests/claude-proxy.yaml.tmpl", vals)...)
	podLabels := labels.Set(job.Spec.Template.Labels)
	// "agent" selects the agent Service (controller.go) and is the security attribution (security.go): not on a chat pod
	if _, ok := podLabels["agent"]; ok {
		t.Errorf("chat pod has the label agent=%q: it would match the Service and the security events of the agent", podLabels["agent"])
	}
	if podLabels["agentdev/chat-agent"] != "alice" {
		t.Errorf("chat pod label agentdev/chat-agent = %q, want alice", podLabels["agentdev/chat-agent"])
	}
	found := map[string]bool{}
	for _, d := range docs {
		var p networkingv1.NetworkPolicy
		if yaml.Unmarshal([]byte(d), &p) != nil || p.Kind != "NetworkPolicy" {
			continue
		}
		match := func(s metav1.LabelSelector) bool {
			sel, err := metav1.LabelSelectorAsSelector(&s)
			return err == nil && sel.Matches(podLabels)
		}
		switch p.Name {
		case "agent-egress":
			found[p.Name] = match(p.Spec.PodSelector)
		case "egress-proxy", "claude-proxy":
			for _, r := range p.Spec.Ingress {
				for _, f := range r.From {
					if f.PodSelector != nil && f.NamespaceSelector == nil && match(*f.PodSelector) {
						found[p.Name] = true
					}
				}
			}
		}
	}
	for _, n := range []string{"agent-egress", "egress-proxy", "claude-proxy"} {
		if !found[n] {
			t.Errorf("NetworkPolicy %s does not cover the chat session pod (%v)", n, podLabels)
		}
	}
}
