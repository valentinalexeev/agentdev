package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func withGo(a *v1.Agent) {
	a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Go: "1.27.4"}}
}

// #499: an agent without spec.toolchain keeps its pod template: no init container, mount, env or volume is added.
func TestToolchainUnsetLeavesPodAlone(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	p := s.Spec.Template.Spec
	if len(p.InitContainers) != 1 || strings.Join(p.InitContainers[0].Command, " ") != "mkdir -p /state/claude" {
		t.Fatalf("init containers changed: %+v", p.InitContainers)
	}
	for _, ct := range p.Containers {
		for _, m := range ct.VolumeMounts {
			if m.MountPath == toolchainDir {
				t.Errorf("%s mounts %s", ct.Name, toolchainDir)
			}
		}
		for _, e := range ct.Env {
			if e.Name == "TOOLCHAIN" || e.Name == "PATH" || e.Name == "CLAUDE_TOOLS_EXTRA" {
				t.Errorf("%s has %s", ct.Name, e.Name)
			}
		}
	}
	for _, v := range p.Volumes {
		if v.Name == "toolchain-tmp" {
			t.Error("toolchain-tmp volume without a toolchain")
		}
	}
}

// #499 (review of the plan): every container that mounts the state volume has /state/toolchain read-only, except the
// init container toolchain that installs it.
func TestToolchainReadOnlyEverywhere(t *testing.T) {
	for _, github := range []bool{false, true} {
		r, c := setup(t, agent("alice", func(a *v1.Agent) {
			withGo(a)
			if github {
				a.Spec.Forge = "github"
			}
		}))
		r.D.EgressProxy, r.D.ClaudeProxyURL = "http://egress-proxy:3128", "http://claude-proxy:8080"
		reconcile(t, r, "alice")
		p := getSTS(t, c, "alice").Spec.Template.Spec
		all := append(append([]corev1.Container{}, p.InitContainers...), p.Containers...)
		checked := 0
		for _, ct := range all {
			usesState, ro := false, false
			for _, m := range ct.VolumeMounts {
				if m.Name == "state" {
					usesState = true
				}
				if m.Name == "state" && m.MountPath == toolchainDir && m.SubPath == "toolchain" && m.ReadOnly {
					ro = true
				}
			}
			switch {
			case !usesState:
			case ct.Name == toolchainContainer:
				if ro {
					t.Errorf("the init container toolchain must write %s", toolchainDir)
				}
			case ct.Name == "state-init":
				// creates the directories before the subPath mounts; runs before anything installs
			default:
				checked++
				if !ro {
					t.Errorf("github=%v: %s mounts the state volume without %s read-only", github, ct.Name, toolchainDir)
				}
			}
		}
		if checked != 2 {
			t.Errorf("github=%v: checked %d containers, want agent and forge", github, checked)
		}
	}
}

// #499: the init container: worker image, UID 1000, read-only root, no capabilities, emptyDir /tmp, the setup proxy
// key and nothing else secret; the agent and forge containers never get the setup proxy key.
func TestToolchainInitContainer(t *testing.T) {
	r, c := setup(t, agent("alice", withGo))
	r.D.EgressProxy, r.D.ClaudeProxyURL = "http://egress-proxy:3128", "http://claude-proxy:8080"
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	p := s.Spec.Template.Spec
	if len(p.InitContainers) != 2 || p.InitContainers[0].Name != "state-init" || p.InitContainers[1].Name != toolchainContainer {
		t.Fatalf("init containers: %+v", p.InitContainers)
	}
	if got := strings.Join(p.InitContainers[0].Command, " "); got != "mkdir -p /state/claude /state/toolchain /state/cache" {
		t.Errorf("state-init: %s", got)
	}
	tc := p.InitContainers[1]
	sc := tc.SecurityContext
	if tc.Image != "worker:dev" || *sc.RunAsUser != 1000 || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(tc.EnvFrom) != 0 {
		t.Errorf("init container shape: %+v", tc)
	}
	tmp := false
	for _, m := range tc.VolumeMounts {
		if m.MountPath == "/tmp" && m.Name == "toolchain-tmp" {
			tmp = true
		}
		if m.Name == proxyTokenVolume || m.Name == "github-app" {
			t.Errorf("init container mounts %s", m.Name)
		}
	}
	if !tmp {
		t.Error("no emptyDir /tmp")
	}
	var specJSON string
	for _, e := range tc.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "agent-alice-setup-proxy" {
			t.Errorf("init container reads Secret %s", e.ValueFrom.SecretKeyRef.Name)
		}
		if e.Name == "HTTPS_PROXY" && e.Value != "http://setup:$(EGRESS_SETUP_KEY)@egress-proxy:3128" {
			t.Errorf("HTTPS_PROXY = %s", e.Value)
		}
		if e.Name == "TOOLCHAIN_SPEC" {
			specJSON = e.Value
		}
	}
	var spec toolchainSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil || spec.V != 1 || spec.Image != "worker:dev" || len(spec.Presets) != 1 ||
		spec.Presets[0].Name != "go" || len(spec.Presets[0].Arch["amd64"].SHA256) != 64 || len(spec.Presets[0].Arch["arm64"].SHA256) != 64 {
		t.Errorf("TOOLCHAIN_SPEC = %s (%v)", specJSON, err)
	}
	for _, ct := range p.Containers {
		for _, e := range ct.Env {
			if e.Name == "EGRESS_SETUP_KEY" || strings.Contains(e.Value, "setup:") ||
				(e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.HasSuffix(e.ValueFrom.SecretKeyRef.Name, "-setup-proxy")) {
				t.Errorf("%s has the setup proxy credential (%s)", ct.Name, e.Name)
			}
		}
	}
	if got := envOf(s, "PATH"); !strings.HasPrefix(got, "/state/toolchain/current/go/bin:") {
		t.Errorf("PATH = %s", got)
	}
	if got := envOf(s, "GOTOOLCHAIN"); got != "local" {
		t.Errorf("GOTOOLCHAIN = %s", got)
	}
}

// #499: the presets' allow entries come first in CLAUDE_TOOLS_EXTRA, then spec.tools.allow.
func TestToolchainAllowMerged(t *testing.T) {
	r, c := setup(t, agent("alice", func(a *v1.Agent) {
		withGo(a)
		a.Spec.Tools = &v1.AgentTools{Allow: []string{"Bash(npm ci:*)"}}
	}))
	reconcile(t, r, "alice")
	got := envOf(getSTS(t, c, "alice"), "CLAUDE_TOOLS_EXTRA")
	if !strings.HasPrefix(got, "Bash(go mod:*),") || !strings.HasSuffix(got, ",Bash(npm ci:*)") {
		t.Errorf("CLAUDE_TOOLS_EXTRA = %s", got)
	}
}

// #499: an unknown version, a path outside $TOOLCHAIN or a refresh under 1h makes the Agent invalid (no pod).
func TestToolchainInvalidRefused(t *testing.T) {
	for name, tc := range map[string]*v1.AgentToolchain{
		"unknown version": {Presets: v1.ToolchainPresets{Go: "1.0.0"}},
		"no preset":       {},
		"path escape":     {Presets: v1.ToolchainPresets{Go: "1.27.4"}, Path: []string{"$TOOLCHAIN/../../usr"}},
		"path absolute":   {Presets: v1.ToolchainPresets{Go: "1.27.4"}, Path: []string{"/tmp/bin"}},
		"short refresh":   {Presets: v1.ToolchainPresets{Go: "1.27.4"}, Refresh: &metav1.Duration{Duration: time.Minute}},
	} {
		r, c := setup(t, agent("mallory", func(a *v1.Agent) { a.Spec.Toolchain = tc }))
		reconcile(t, r, "mallory")
		var s appsv1.StatefulSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-mallory"}, &s); err == nil {
			t.Errorf("%s: rendered a pod", name)
		}
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "mallory"}, &a)
		if a.Status.Phase != "invalid" {
			t.Errorf("%s: phase = %q, want invalid", name, a.Status.Phase)
		}
	}
}

// #499: the refresh epoch differs per agent (offset) and changes exactly once per period.
func TestToolchainRefreshEpoch(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	e1, next := refreshEpoch("alice", 168*time.Hour, now)
	if !next.After(now) || next.Sub(now) > 168*time.Hour {
		t.Fatalf("next = %s", next)
	}
	if e, _ := refreshEpoch("alice", 168*time.Hour, next.Add(-time.Second)); e != e1 {
		t.Error("epoch changed before next")
	}
	if e, _ := refreshEpoch("alice", 168*time.Hour, next); e != e1+1 {
		t.Error("epoch did not change at next")
	}
	_, nextBob := refreshEpoch("bob", 168*time.Hour, now)
	if nextBob.Equal(next) {
		t.Error("alice and bob refresh at the same moment")
	}
	if e, n := refreshEpoch("alice", 0, now); e != 0 || !n.IsZero() {
		t.Error("no refresh must give epoch 0")
	}
}

// #499: the operator reads the termination message into status.toolchain, keeps the hash for the next start, and a
// Reinstalled report (changed manifest) is a high-severity security event. A failure is shown, nothing is recorded.
func TestToolchainStatusFromTerminationMessage(t *testing.T) {
	key, hash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		msg, reason string
		recorded    bool
		event       bool
	}{
		{`{"key":"` + key + `","hash":"` + hash + `","reason":"Installed","message":"ok"}`, "Installed", true, false},
		{`{"key":"` + key + `","hash":"` + hash + `","reason":"Reinstalled","message":"changed"}`, "Reinstalled", true, true},
		{`{"key":"","hash":"","reason":"StepFailed","message":"go: sha256 mismatch"}`, "StepFailed", false, false},
		{`garbage`, "StepFailed", false, false},
		{`{"key":"../x","hash":"` + hash + `","reason":"Installed"}`, "Invalid", false, false},
	} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-alice-0", Namespace: ns, UID: "pod-1",
			Labels: map[string]string{"app": "coding-agent", "agent": "alice"}},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: toolchainContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: tc.msg}}}}}}
		r, c := setup(t, agent("alice", withGo), pod)
		reconcile(t, r, "alice")
		var a v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "alice"}, &a)
		if a.Status.Toolchain == nil || a.Status.Toolchain.Reason != tc.reason {
			t.Errorf("%s: status.toolchain = %+v, want %s", tc.msg, a.Status.Toolchain, tc.reason)
		}
		var cm corev1.ConfigMap
		err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: toolchainsCM}, &cm)
		if got := err == nil && cm.Data["alice"] == key+":"+hash; got != tc.recorded {
			t.Errorf("%s: recorded = %v, want %v", tc.msg, got, tc.recorded)
		}
		var evs v1.SecurityEventList
		_ = c.List(context.Background(), &evs)
		high := len(evs.Items) == 1 && evs.Items[0].Spec.Rule == "toolchain-manifest" && evs.Items[0].Spec.Severity == "high"
		if high != tc.event {
			t.Errorf("%s: event = %+v, want %v", tc.msg, evs.Items, tc.event)
		}
	}
}

// #499: the setup proxy key is copied from egress-setup-proxy only for an agent with a toolchain behind the proxy.
func TestSetupProxyKey(t *testing.T) {
	src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: setupProxySecret, Namespace: ns},
		Data: map[string][]byte{"password": []byte("0123456789abcdef0123")}}
	r, c := setup(t, agent("alice", withGo), agent("bob", nil), src)
	r.D.EgressProxy = "http://egress-proxy:3128"
	reconcile(t, r, "alice")
	reconcile(t, r, "bob")
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice-setup-proxy"}, &s); err != nil ||
		string(s.Data["password"]) != "0123456789abcdef0123" {
		t.Errorf("alice setup key: %v %v", err, s.Data)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-bob-setup-proxy"}, &s); err == nil {
		t.Error("bob has no toolchain but got a setup key")
	}
}

// #499: every preset of the catalogue has both architectures with an https URL and a SHA-256, and download hosts.
func TestToolchainCatalogue(t *testing.T) {
	for name, vs := range toolchainCatalogue {
		for v, p := range vs {
			if p.Name != name || p.Version != v || len(p.Verify) == 0 || len(p.hosts) == 0 || p.Bin == "" {
				t.Errorf("%s %s: incomplete", name, v)
			}
			for _, a := range []string{"amd64", "arm64"} {
				d := p.Arch[a]
				if !strings.HasPrefix(d.URL, "https://") || len(d.SHA256) != 64 {
					t.Errorf("%s %s %s: %+v", name, v, a, d)
				}
				host := strings.SplitN(strings.TrimPrefix(d.URL, "https://"), "/", 2)[0]
				found := false
				for _, h := range p.hosts {
					found = found || h == host
				}
				if !found {
					t.Errorf("%s %s: host %s not in hosts", name, v, host)
				}
			}
		}
	}
	if got := strings.Join(ToolchainHosts(), " "); got != "dl.google.com static.rust-lang.org" {
		t.Errorf("hosts = %s", got)
	}
}
