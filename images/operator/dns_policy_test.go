package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// nodeLocalDNSIP is the link-local address of GKE's NodeLocal DNSCache.
const nodeLocalDNSIP = "169.254.20.10/32"

// allPolicies renders every manifests/*.tmpl (each variable set to "x") and returns its NetworkPolicies by file/name.
func allPolicies(t *testing.T) map[string]networkingv1.NetworkPolicy {
	t.Helper()
	files, err := filepath.Glob("../../manifests/*.tmpl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests: %v", err)
	}
	out := map[string]networkingv1.NetworkPolicy{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range strings.Split(os.Expand(string(b), func(string) string { return "x" }), "\n---\n") {
			if !strings.Contains(d, "kind: NetworkPolicy") {
				continue
			}
			var p networkingv1.NetworkPolicy
			if err := yaml.Unmarshal([]byte(d), &p); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out[filepath.Base(f)+"/"+p.Name] = p
		}
	}
	return out
}

func isDNSOnly(r networkingv1.NetworkPolicyEgressRule) bool {
	if len(r.Ports) == 0 {
		return false
	}
	for _, p := range r.Ports {
		if p.Port == nil || p.Port.IntValue() != 53 {
			return false
		}
	}
	return true
}

// #163: every policy that allows DNS to kube-dns also allows it to the NodeLocal DNSCache (pods and link-local IP),
// on port 53 only, and the node-local destinations appear in no other rule.
func TestDNSPoliciesAllowNodeLocalDNS(t *testing.T) {
	n := 0
	for name, p := range allPolicies(t) {
		for _, r := range p.Spec.Egress {
			var kubeDNS, nodeLocal, ip bool
			for _, to := range r.To {
				if to.PodSelector != nil {
					switch to.PodSelector.MatchLabels["k8s-app"] {
					case "kube-dns":
						kubeDNS = true
					case "node-local-dns":
						nodeLocal = true
					}
				}
				if to.IPBlock != nil && to.IPBlock.CIDR == nodeLocalDNSIP {
					ip = true
					if len(to.IPBlock.Except) != 0 {
						t.Errorf("%s: unexpected except on the node-local IP", name)
					}
				}
			}
			if (nodeLocal || ip || kubeDNS) && !isDNSOnly(r) {
				t.Errorf("%s: a DNS destination is allowed on a port other than 53", name)
			}
			if kubeDNS {
				n++
				if !nodeLocal || !ip {
					t.Errorf("%s: DNS to kube-dns but not to the NodeLocal DNSCache (node-local-dns pods and %s)", name, nodeLocalDNSIP)
				}
			}
		}
	}
	if n < 5 { // egress-proxy-dns, agent-egress, claude-proxy, executor-egress-proxy-dns, executor-egress
		t.Errorf("found %d DNS rules, want at least 5", n)
	}
}
