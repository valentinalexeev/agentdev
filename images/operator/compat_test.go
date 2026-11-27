package main

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Mixed-version rule (ADR #178): during a rollout a newer operator only ADDS to
// an agent pod template. It never removes or renames a container, an env var, a
// volume or a volume mount that the previous release rendered.
//
// The golden is the pod contract the previous release (goldenRelease) rendered
// for the fixture agent below. Regenerate it from a checkout of that release:
//
//	git worktree add /tmp/prev v1.3.0
//	cp images/operator/compat_test.go /tmp/prev/images/operator/
//	(cd /tmp/prev/images/operator && go test -count=1 -run TestPodTemplateIsAdditive -update .)
//	cp /tmp/prev/images/operator/testdata/pod-contract-v1.3.0.txt images/operator/testdata/
//
// After a release, bump goldenRelease and regenerate the same way from the new tag.
var updateGolden = flag.Bool("update", false, "rewrite testdata/pod-contract-<release>.txt from the current render")

const goldenRelease = "v1.3.0"

// podContract lists the names a running pod of the previous version depends on,
// one per line, sorted.
func podContract(p corev1.PodSpec) []string {
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
	add("initContainer", p.InitContainers)
	add("container", p.Containers)
	for _, v := range p.Volumes {
		out = append(out, "volume "+v.Name)
	}
	sort.Strings(out)
	return out
}

func TestPodTemplateIsAdditive(t *testing.T) {
	r, c := setup(t, agent("compat", nil))
	reconcile(t, r, "compat")
	have := podContract(getSTS(t, c, "compat").Spec.Template.Spec)

	path := filepath.Join("testdata", "pod-contract-"+goldenRelease+".txt")
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
}
