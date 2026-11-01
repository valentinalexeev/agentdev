package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
