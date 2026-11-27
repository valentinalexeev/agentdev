package main

import (
	"strings"
	"testing"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// #251 part 3: every container of an agent pod has an ephemeral-storage limit, the mirror emptyDir a size limit
func TestAgentPodEphemeralStorageLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*v1.Agent)
		init int
	}{{"plain", nil, 1}, {"go", withGo, 2}, {"rust", func(a *v1.Agent) {
		a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Rust: "1.100.0"}}
	}, 2}} {
		t.Run(tc.name, func(t *testing.T) { ephemeralLimits(t, tc.mut, tc.init) })
	}
}

func ephemeralLimits(t *testing.T, mut func(*v1.Agent), inits int) {
	r, c := setup(t, agent("olga", mut))
	reconcile(t, r, "olga")
	spec := getSTS(t, c, "olga").Spec.Template.Spec
	if len(spec.InitContainers) != inits {
		t.Fatalf("init containers %d, want %d", len(spec.InitContainers), inits)
	}
	want := map[string]string{"forge": defForgeMirrorLimit, "toolchain-tmp": toolchainTmpLimit}
	for _, ct := range append(spec.InitContainers, spec.Containers...) {
		l, ok := ct.Resources.Limits["ephemeral-storage"]
		if !ok || l.IsZero() {
			t.Errorf("%s: no ephemeral-storage limit", ct.Name)
		}
		if q, ok := ct.Resources.Requests["ephemeral-storage"]; !ok || q.Cmp(l) > 0 {
			t.Errorf("%s: request %v above limit %v", ct.Name, q, l)
		}
	}
	for _, v := range spec.Volumes {
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(want[v.Name])) != 0) {
			t.Errorf("emptyDir %s: size limit %v", v.Name, v.EmptyDir.SizeLimit)
		}
	}
	// the toolchain init container's limit holds its whole /tmp (archive, rustup-init work files)
	for _, ct := range spec.InitContainers {
		if ct.Name == toolchainContainer {
			l := ct.Resources.Limits[corev1.ResourceEphemeralStorage]
			if l.Cmp(resource.MustParse(toolchainTmpLimit)) <= 0 {
				t.Errorf("toolchain limit %s does not exceed its /tmp %s", l.String(), toolchainTmpLimit)
			}
		}
	}
}

// #251: Go's build cache is on the state volume, not the writable layer counted against the ephemeral limit
func TestGoBuildCacheOnStateVolume(t *testing.T) {
	r, c := setup(t, agent("olga", withGo))
	reconcile(t, r, "olga")
	for _, ct := range getSTS(t, c, "olga").Spec.Template.Spec.Containers {
		if ct.Name != "agent" {
			continue
		}
		for _, e := range ct.Env {
			if e.Name == "GOCACHE" && e.Value == "/state/cache/go/build" {
				return
			}
		}
		t.Errorf("agent env has no GOCACHE on /state: %v", ct.Env)
	}
}

// the limits are settings (#265 review): configured values are used, empty or invalid ones give the defaults
func TestAgentPodEphemeralStorageConfigurable(t *testing.T) {
	r, c := setup(t, agent("olga", nil))
	r.D.AgentEphemeralRequest, r.D.AgentEphemeralLimit, r.D.ForgeMirrorLimit, r.D.ForgeEphemeralLimit = "2Gi", "20Gi", "bad", ""
	reconcile(t, r, "olga")
	spec := getSTS(t, c, "olga").Spec.Template.Spec
	want := map[string][2]string{"agent": {"2Gi", "20Gi"}, "forge": {"256Mi", defForgeEphemeralLimit}}
	for _, ct := range spec.Containers {
		w, ok := want[ct.Name]
		if !ok {
			continue
		}
		q, l := ct.Resources.Requests["ephemeral-storage"], ct.Resources.Limits["ephemeral-storage"]
		if q.String() != w[0] || l.String() != w[1] {
			t.Errorf("%s: request %s limit %s, want %v", ct.Name, q.String(), l.String(), w)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == "forge" && v.EmptyDir.SizeLimit.String() != defForgeMirrorLimit {
			t.Errorf("mirror limit %v, want the default for an invalid value", v.EmptyDir.SizeLimit)
		}
	}
}

// #251 follow-up: FORGE_EPHEMERAL_LIMIT below FORGE_MIRROR_LIMIT is refused (on the effective values)
func TestValidateEphemeral(t *testing.T) {
	for _, tc := range []struct {
		mirror, limit string
		ok            bool
	}{
		{"", "", true},        // defaults 4Gi / 5Gi
		{"4Gi", "4Gi", true},  // equal is allowed
		{"1Gi", "bad", true},  // invalid limit: default 5Gi
		{"4Gi", "3Gi", false}, // below the mirror
		{"8Gi", "", false},    // default 5Gi below a raised mirror
		{"bad", "1Gi", false}, // invalid mirror: default 4Gi
	} {
		err := Defaults{ForgeMirrorLimit: tc.mirror, ForgeEphemeralLimit: tc.limit}.validateEphemeral()
		if (err == nil) != tc.ok {
			t.Errorf("mirror %q limit %q: err %v, want ok=%v", tc.mirror, tc.limit, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "FORGE_EPHEMERAL_LIMIT") {
			t.Errorf("error %q does not name the setting", err)
		}
	}
}
