package main

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// The pod template is internal interface (ADR 0006): an agent pod of the previous MINOR release runs next to this
// operator during a rollout, and a newer worker reads what an older operator rendered. Names may be added, never
// removed or renamed within one MINOR step. testdata/pod-contract.txt is the list the last release rendered; a new
// name is added to it in the PR that adds it, a removal needs the two-release path of ADR 0006 (and an edit here
// that says which release drops it).
func podContract(s *appsv1.StatefulSet) []string {
	var out []string
	p := s.Spec.Template.Spec
	for _, c := range p.InitContainers {
		out = append(out, "init "+c.Name)
	}
	for _, c := range p.Containers {
		out = append(out, "container "+c.Name)
		for _, e := range c.Env {
			out = append(out, "env "+c.Name+" "+e.Name)
		}
		for _, m := range c.VolumeMounts {
			out = append(out, "mount "+c.Name+" "+m.MountPath)
		}
	}
	for _, v := range p.Volumes {
		out = append(out, "volume "+v.Name)
	}
	sort.Strings(out)
	return out
}

func TestPodTemplateIsAdditive(t *testing.T) {
	// every variant the operator can render: Gitea and GitHub, egress and Claude proxy on and off
	have := map[string]bool{}
	for _, d := range []func(*Defaults){
		func(*Defaults) {},
		func(d *Defaults) { d.EgressProxy, d.ClaudeProxyURL = "http://egress:3128", "http://claude-proxy" },
	} {
		for _, forge := range []string{"gitea", "github"} {
			r, c := setup(t, agent("alice", func(a *v1.Agent) { a.Spec.Forge = forge }))
			d(&r.D)
			reconcile(t, r, "alice")
			for _, k := range podContract(getSTS(t, c, "alice")) {
				have[k] = true
			}
		}
	}
	f, err := os.Open("testdata/pod-contract.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !have[line] {
			t.Errorf("pod template no longer renders %q: an agent pod of the previous release may depend on it (ADR 0006)", line)
		}
	}
}
