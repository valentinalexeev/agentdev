package main

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Mixed-version rule (ADR #178): during a rollout a newer operator only ADDS to
// an agent pod template. It never removes or renames a container, an env var, a
// volume, a volume mount or a volume claim template that the previous release
// rendered.
//
// The goldens are the pod contracts the previous release (goldenRelease)
// rendered for the fixtures below, one file per fixture. Regenerate them from a
// checkout of that release:
//
//	git worktree add /tmp/prev v1.3.0
//	cp images/operator/compat_test.go /tmp/prev/images/operator/
//	(cd /tmp/prev/images/operator && go test -count=1 -run TestPodTemplateIsAdditive -update .)
//	cp /tmp/prev/images/operator/testdata/pod-contract-v1.3.0*.txt images/operator/testdata/
//
// After a release, bump goldenRelease and regenerate the same way from the new tag.
var updateGolden = flag.Bool("update", false, "rewrite testdata/pod-contract-<release>*.txt from the current render")

const goldenRelease = "v1.3.0"

// compatFixtures: the agents whose pod contract must stay. "" is the minimal
// agent (gitea, no proxies); "full" turns on the optional features (#613).
var compatFixtures = []struct {
	name string
	mut  func(*v1.Agent)
	defs func(*Defaults)
}{
	{"", nil, nil},
	{"full", func(a *v1.Agent) {
		a.Spec.Forge, a.Spec.Repo = "github", "octo-org/repo"
		a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Go: "1.27.4", Rust: "1.100.0"}}
	}, func(d *Defaults) {
		d.GitHubURL = "https://github.com"
		d.EgressProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"
		d.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
		d.AgentEgressHosts = "registry.npmjs.org .github.com pypi.org"
		d.Approvals, d.ApprovalsURL, d.ApprovalTimeout = "true", "http://agentdev-api-internal:8081", "30"
	}},
}

// podContract lists the names a running pod of the previous version depends on,
// one per line, sorted.
func podContract(s appsv1.StatefulSetSpec) []string {
	var out []string
	add := func(kind string, cs []corev1.Container) {
		for _, c := range cs {
			out = append(out, kind+" "+c.Name)
			for _, e := range c.Env {
				out = append(out, kind+" "+c.Name+" env "+e.Name)
			}
			for _, m := range c.VolumeMounts {
				out = append(out, kind+" "+c.Name+" mount "+m.Name+" "+m.MountPath)
			}
		}
	}
	p := s.Template.Spec
	add("initContainer", p.InitContainers)
	add("container", p.Containers)
	for _, v := range p.Volumes {
		out = append(out, "volume "+v.Name)
	}
	for _, c := range s.VolumeClaimTemplates {
		out = append(out, "volumeClaimTemplate "+c.Name)
	}
	sort.Strings(out)
	return out
}

func TestPodTemplateIsAdditive(t *testing.T) {
	for _, fx := range compatFixtures {
		name := "compat"
		file := "pod-contract-" + goldenRelease + ".txt"
		if fx.name != "" {
			name += "-" + fx.name
			file = "pod-contract-" + goldenRelease + "-" + fx.name + ".txt"
		}
		t.Run(name, func(t *testing.T) {
			r, c := setup(t, agent(name, fx.mut))
			if fx.defs != nil {
				fx.defs(&r.D)
			}
			reconcile(t, r, name)
			have := podContract(getSTS(t, c, name).Spec)

			path := filepath.Join("testdata", file)
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Join(have, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with -update from %s): %v", goldenRelease, err)
			}
			cur := map[string]bool{}
			for _, l := range have {
				cur[l] = true
			}
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if l != "" && !cur[l] {
					t.Errorf("pod template drops %q rendered by %s: a newer operator may only add (ADR #178)", l, goldenRelease)
				}
			}
		})
	}
}
