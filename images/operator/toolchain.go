package main

// Toolchains per agent (#499, ADR 0010 D2 presets, D3 layer 1). The Agent's spec.toolchain names preset versions of the
// catalogue below. The init container toolchain (worker image, images/worker/toolchain-install.sh) downloads them, checks
// the pinned SHA-256 and installs them into /state/toolchain/<key> on the agent's volume. Every other container that
// mounts the volume gets /state/toolchain read-only (a separate subPath mount over the read-write /state), so neither
// Claude nor the repository's code can rewrite bin/go for the next pod. As a second check the init container hashes the
// installed tree on every start and compares it with the hash it reported last time (TOOLCHAIN_EXPECT, from the
// ConfigMap agentdev-toolchains, read at pod start so a new hash does not change the pod template). A mismatch is a
// security event and the tree is installed again.
//
// Setup egress (R10): the download hosts are open only with the setup proxy credential, Secret
// agent-<name>-setup-proxy, which only the init container gets (like egress-forge-proxy for the forge sidecar). The
// operator copies it from the instance's egress-setup-proxy. With presets only, the hosts are the same for every
// agent, so one Squid ACL serves all; per-agent hosts come with free-form steps (ADR 0010 D4 step 3).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,resourceNames=egress-setup-proxy
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,resourceNames=agentdev-toolchains

const (
	toolchainDir        = "/state/toolchain"
	toolchainContainer  = "toolchain"
	toolchainsCM        = "agentdev-toolchains" // key per agent: "<key>:<manifest hash>" of the last good start
	setupProxySecret    = "egress-setup-proxy"  // the instance's setup proxy credential (password, auth)
	toolchainSubPath    = "toolchain"
	toolchainMinRefresh = time.Hour
)

// download of one preset for one architecture
type download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// preset is one catalogue entry: everything the init container needs, and what the agent gets.
type preset struct {
	Name    string              `json:"name"`
	Version string              `json:"version"`
	Kind    string              `json:"kind"` // tar | rustup (images/worker/toolchain-install.sh)
	Arch    map[string]download `json:"arch"`
	Bin     string              `json:"bin"`    // PATH entry below the toolchain, also for verify
	Verify  []string            `json:"verify"` // must exit 0 after the install
	hosts   []string            // egress hosts of the download (egress-setup-proxy ACL, manifests/egress.yaml.tmpl)
	allow   []string            // added to CLAUDE_TOOLS_EXTRA
	env     map[string]string   // agent container environment
}

// toolchainCatalogue: name -> version -> preset. Adding a version or a preset is an agentdev release. The SHA-256 values
// are the ones published by go.dev and static.rust-lang.org; rustup-init 1.29.3 installs the pinned Rust toolchain.
var toolchainCatalogue = map[string]map[string]preset{
	"go": {
		"1.27.4": goPreset("1.27.4", "7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840",
			"3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92"),
		"1.26.11": goPreset("1.26.11", "9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec",
			"6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e"),
	},
	"rust": {
		"1.100.0": rustPreset("1.100.0"),
	},
}

func goPreset(v, amd64, arm64 string) preset {
	u := "https://dl.google.com/go/go" + v + ".linux-"
	return preset{Name: "go", Version: v, Kind: "tar", Bin: "go/bin", Verify: []string{"go", "version"},
		Arch:  map[string]download{"amd64": {u + "amd64.tar.gz", amd64}, "arm64": {u + "arm64.tar.gz", arm64}},
		hosts: []string{"dl.google.com"},
		allow: []string{"Bash(go mod:*)", "Bash(gofmt:*)", "Bash(go fmt:*)", "Bash(go run:*)"},
		// GOTOOLCHAIN=local: go never downloads another toolchain on its own; module downloads stay on the agent's disk
		env: map[string]string{"GOTOOLCHAIN": "local", "GOPATH": "/state/cache/go", "GOMODCACHE": "/state/cache/go/mod"}}
}

func rustPreset(v string) preset {
	const r = "https://static.rust-lang.org/rustup/archive/1.29.3/"
	return preset{Name: "rust", Version: v, Kind: "rustup", Bin: "cargo/bin", Verify: []string{"cargo", "--version"},
		Arch: map[string]download{
			"amd64": {r + "x86_64-unknown-linux-gnu/rustup-init", "b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55"},
			"arm64": {r + "aarch64-unknown-linux-gnu/rustup-init", "9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282"}},
		hosts: []string{"static.rust-lang.org"},
		allow: []string{"Bash(cargo build:*)", "Bash(cargo check:*)", "Bash(cargo fetch:*)", "Bash(cargo run:*)"},
		// the toolchain is read-only; the registry and downloads live in the agent's own cache
		env: map[string]string{"RUSTUP_HOME": toolchainDir + "/current/rustup", "CARGO_HOME": "/state/cache/cargo",
			"RUSTUP_TOOLCHAIN": v}}
}

// ToolchainHosts lists the download hosts of every preset (for the setup proxy ACL and the docs).
func ToolchainHosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, vs := range toolchainCatalogue {
		for _, p := range vs {
			for _, h := range p.hosts {
				if !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

var toolchainPathRE = regexp.MustCompile(`^\$TOOLCHAIN(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

// presetsOf: the catalogue entries of the Agent, in a fixed order (go, rust). ok=false names the first unknown version.
func presetsOf(t *v1.AgentToolchain) ([]preset, string) {
	var out []preset
	for _, pv := range []struct{ name, version string }{{"go", t.Presets.Go}, {"rust", t.Presets.Rust}} {
		if pv.version == "" {
			continue
		}
		p, ok := toolchainCatalogue[pv.name][pv.version]
		if !ok {
			return nil, fmt.Sprintf("spec.toolchain.presets.%s: version %q is not in the catalogue of this agentdev release", pv.name, pv.version)
		}
		out = append(out, p)
	}
	return out, ""
}

// toolchainValid is the operator's check behind the CRD rules (#499): "" when spec.toolchain is unset or valid.
func toolchainValid(a *v1.Agent) string {
	t := a.Spec.Toolchain
	if t == nil {
		return ""
	}
	ps, msg := presetsOf(t)
	if msg != "" {
		return msg
	}
	if len(ps) == 0 {
		return "spec.toolchain.presets needs at least one preset"
	}
	if len(t.Path) > 10 {
		return "spec.toolchain.path: more than 10 entries"
	}
	for _, p := range t.Path {
		if len(p) > 200 || !toolchainPathRE.MatchString(p) {
			return fmt.Sprintf("spec.toolchain.path: %q is not $TOOLCHAIN or a path below it", p)
		}
	}
	if t.Refresh != nil && t.Refresh.Duration < toolchainMinRefresh {
		return "spec.toolchain.refresh: at least 1h"
	}
	return ""
}

// refreshEpoch: floor((now + offset) / refresh), offset from a hash of the agent name so agents do not all refresh at
// the same moment (ADR 0010 D2). next is when the epoch changes. 0 and the zero time without refresh.
func refreshEpoch(agent string, refresh time.Duration, now time.Time) (int64, time.Time) {
	if refresh <= 0 {
		return 0, time.Time{}
	}
	h := fnv.New64a()
	h.Write([]byte(agent))
	offset := int64(h.Sum64() % uint64(refresh))
	t := now.UnixNano() + offset
	epoch := t / int64(refresh)
	next := time.Unix(0, (epoch+1)*int64(refresh)-offset)
	return epoch, next
}

// toolchainSpec is the canonical spec the init container installs and hashes into the key: presets, path, worker image
// and refresh epoch. It goes into the pod template, so a change of any of them installs again (after drain).
type toolchainSpec struct {
	V     int    `json:"v"`
	Image string `json:"image"`
	// Digest (#538, ADR 0010 D3): the worker image digest when it differs from the first one seen for this tag (a
	// re-pushed tag). Empty otherwise, so the spec of an agent whose tag did not move is unchanged.
	Digest  string   `json:"digest,omitempty"`
	Epoch   int64    `json:"epoch"`
	Path    []string `json:"path"`
	Presets []preset `json:"presets"`
}

func (r *AgentReconciler) toolchainSpecJSON(a *v1.Agent, ps []preset, digest string) string {
	var refresh time.Duration
	if a.Spec.Toolchain.Refresh != nil {
		refresh = a.Spec.Toolchain.Refresh.Duration
	}
	epoch, _ := refreshEpoch(a.Name, refresh, r.now())
	path := append([]string{}, a.Spec.Toolchain.Path...)
	if path == nil {
		path = []string{}
	}
	b, _ := json.Marshal(toolchainSpec{V: 1, Image: r.D.WorkerImage, Digest: digest, Epoch: epoch, Path: path, Presets: ps})
	return string(b)
}

// Worker image digest of the toolchain key (#538, ADR 0010 D3). A tag such as :latest can be pushed again with other
// content; the install must then run again. The operator has no registry access (no egress, no pull secret, and kind
// loads images without a registry), so the digest comes from the kubelet: the imageID of the toolchain init container
// of the agent's pod, which is the image that really ran the install. Two StatefulSet annotations keep it:
//   - toolchainBaseAnnotation: "<image ref> <digest>", the first digest seen for the current WORKER_IMAGE. The
//     install keyed on the tag alone was made with it, so it does not go into the spec (no extra roll on an upgrade).
//   - toolchainDigestAnnotation: the latest digest seen for that image ref.
//
// When the latest differs from the base, the spec gets "digest": the pod template changes, the pod is replaced after
// drain (#174), and the new pod installs under a new key. Fail-safe: without a pod, a status or a valid digest nothing
// changes (the key stays the tag's, as before). Without spec.toolchain nothing is read or written.
const (
	toolchainBaseAnnotation   = "agentdev/toolchain-image-base"
	toolchainDigestAnnotation = "agentdev/toolchain-image-digest"
)

var imageDigest = regexp.MustCompile(`(sha256:[0-9a-f]{64})$`)

// observedToolchainDigest: the digest of the image the toolchain init container of p ran, when it ran image. "" when
// unknown (no status yet, an old pod of another image, or an imageID without a sha256 digest).
func observedToolchainDigest(p *corev1.Pod, image string) string {
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.Name != toolchainContainer || cs.ImageID == "" {
			continue
		}
		// the status image of the pod's own spec; an older pod of the previous tag is not this image's evidence
		for _, c := range p.Spec.InitContainers {
			if c.Name == toolchainContainer && c.Image != image {
				return ""
			}
		}
		if m := imageDigest.FindStringSubmatch(cs.ImageID); m != nil {
			return m[1]
		}
	}
	return ""
}

// noteToolchainDigest records an observed digest on the StatefulSet annotations (see above). Called in the mutate
// function before render; d == "" or no toolchain changes nothing.
func (r *AgentReconciler) noteToolchainDigest(a *v1.Agent, s *appsv1.StatefulSet, d string) {
	if a.Spec.Toolchain == nil || d == "" {
		return
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	img, base, _ := strings.Cut(s.Annotations[toolchainBaseAnnotation], " ")
	if img != r.D.WorkerImage || base == "" {
		s.Annotations[toolchainBaseAnnotation] = r.D.WorkerImage + " " + d
	}
	s.Annotations[toolchainDigestAnnotation] = d
}

// toolchainDigest: the digest for the spec, "" while the image ran with its base digest or nothing is known.
func (r *AgentReconciler) toolchainDigest(s *appsv1.StatefulSet) string {
	img, base, _ := strings.Cut(s.Annotations[toolchainBaseAnnotation], " ")
	d := s.Annotations[toolchainDigestAnnotation]
	if img != r.D.WorkerImage || base == "" || !imageDigest.MatchString(d) || d == base {
		return ""
	}
	return d
}

// toolchainPodDigest reads the agent pod for noteToolchainDigest. Errors are not fatal: "" keeps the current key.
func (r *AgentReconciler) toolchainPodDigest(ctx context.Context, a *v1.Agent) string {
	if a.Spec.Toolchain == nil {
		return ""
	}
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: "agent-" + a.Name + "-0"}, &pod); err != nil {
		return ""
	}
	return observedToolchainDigest(&pod, r.D.WorkerImage)
}

// toolchainNextRefresh: how long until the agent's refresh epoch changes; 0 without refresh.
func (r *AgentReconciler) toolchainNextRefresh(a *v1.Agent) time.Duration {
	if a.Spec.Toolchain == nil || a.Spec.Toolchain.Refresh == nil {
		return 0
	}
	_, next := refreshEpoch(a.Name, a.Spec.Toolchain.Refresh.Duration, r.now())
	if d := next.Sub(r.now()); d > 0 {
		return d + time.Second
	}
	return time.Second
}

// toolchainAllow: the allow entries of the Agent's presets (#499), added to CLAUDE_TOOLS_EXTRA.
func toolchainAllow(a *v1.Agent) []string {
	if a.Spec.Toolchain == nil {
		return nil
	}
	ps, _ := presetsOf(a.Spec.Toolchain)
	var out []string
	for _, p := range ps {
		out = append(out, p.allow...)
	}
	return out
}

// imagePath is the worker image's PATH (node:24-bookworm-slim); the toolchain entries go in front of it.
const imagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// toolchainAgentEnv: TOOLCHAIN, PATH and the presets' variables for the agent container. Nothing without a toolchain.
func toolchainAgentEnv(a *v1.Agent) []corev1.EnvVar {
	if a.Spec.Toolchain == nil {
		return nil
	}
	ps, _ := presetsOf(a.Spec.Toolchain)
	cur := toolchainDir + "/current"
	var path []string
	for _, p := range a.Spec.Toolchain.Path {
		path = append(path, strings.Replace(p, "$TOOLCHAIN", cur, 1))
	}
	vars := map[string]string{}
	for _, p := range ps {
		path = append(path, cur+"/"+p.Bin)
		for k, v := range p.env {
			vars[k] = v
		}
	}
	out := []corev1.EnvVar{{Name: "TOOLCHAIN", Value: cur}, {Name: "PATH", Value: strings.Join(append(path, imagePath), ":")}}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: vars[k]})
	}
	return out
}

// toolchainMount: /state/toolchain read-only, for every container but the init container (ADR 0010 D3).
func toolchainMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "state", MountPath: toolchainDir, SubPath: toolchainSubPath, ReadOnly: true}
}

// setupProxySecretName: the agent's setup proxy credential, mounted only in the init container.
func setupProxySecretName(agent string) string { return "agent-" + agent + "-setup-proxy" }

// toolchainInit is the init container toolchain: worker image, UID 1000, no capabilities, read-only root, an emptyDir
// /tmp, no forge or Claude credential and no service account token; only the setup proxy credential.
func (r *AgentReconciler) toolchainInit(a *v1.Agent, ps []preset, digest string) corev1.Container {
	env := []corev1.EnvVar{{Name: "TOOLCHAIN_SPEC", Value: r.toolchainSpecJSON(a, ps, digest)}, {Name: "TOOLCHAIN_DIR", Value: toolchainDir},
		{Name: "TMPDIR", Value: "/tmp"}, {Name: "HOME", Value: "/tmp"},
		// the last good "<key>:<hash>", read at pod start (a new value does not change the pod template)
		{Name: "TOOLCHAIN_EXPECT", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: toolchainsCM}, Key: a.Name, Optional: ptr.To(true)}}}}
	if r.D.EgressProxy != "" {
		env = append(env, corev1.EnvVar{Name: "EGRESS_SETUP_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: setupProxySecretName(a.Name)}, Key: "password"}}})
		proxy := strings.Replace(r.D.EgressProxy, "://", "://setup:$(EGRESS_SETUP_KEY)@", 1)
		for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			env = append(env, corev1.EnvVar{Name: k, Value: proxy})
		}
		noProxy := "localhost,127.0.0.1,::1,.svc,.svc.cluster.local,.cluster.local"
		env = append(env, corev1.EnvVar{Name: "NO_PROXY", Value: noProxy}, corev1.EnvVar{Name: "no_proxy", Value: noProxy})
	}
	return corev1.Container{
		Name: toolchainContainer, Image: r.D.WorkerImage, ImagePullPolicy: corev1.PullPolicy(r.D.PullPolicy),
		TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Command: []string{"toolchain-install"},
		Env:     env,
		VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: "/state"},
			{Name: "toolchain-tmp", MountPath: "/tmp"}},
		SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000), RunAsNonRoot: ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
	}
}

func toolchainTmpVolume() corev1.Volume {
	return corev1.Volume{Name: "toolchain-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
		SizeLimit: ptr.To(resource.MustParse("2Gi"))}}}
}

// ensureSetupProxyKey writes agent-<name>-setup-proxy from the instance's egress-setup-proxy for an agent with a
// toolchain behind the egress proxy. Without the instance Secret nothing is written: the init container's secretKeyRef
// then keeps the pod Pending (fail closed). The same value for every agent while the hosts are the presets' only.
func (r *AgentReconciler) ensureSetupProxyKey(ctx context.Context, a *v1.Agent) error {
	if a.Spec.Toolchain == nil || r.D.EgressProxy == "" {
		return nil
	}
	rd := r.Direct
	if rd == nil {
		rd = r.Client
	}
	var src corev1.Secret
	if err := rd.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: setupProxySecret}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("setup proxy secret: %w", err)
	}
	pw := strings.TrimSpace(string(src.Data["password"]))
	if len(pw) < 16 {
		return fmt.Errorf("secret %s: password missing or shorter than 16 characters", setupProxySecret)
	}
	ctrl := true
	s := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: setupProxySecretName(a.Name), Namespace: a.Namespace,
			Labels: map[string]string{"app": "coding-agent", "agent": a.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "Agent", Name: a.Name,
				UID: a.UID, Controller: &ctrl, BlockOwnerDeletion: &ctrl}}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte(pw)},
	}
	//nolint:staticcheck // same server-side apply as ensureProxyKey
	if err := r.Client.Patch(ctx, s, client.Apply, client.FieldOwner("agentdev-operator"), client.ForceOwnership); err != nil {
		return fmt.Errorf("setup proxy key secret: %w", err)
	}
	return nil
}

// toolchainReport reads the init container's termination message of the agent pod (#499): the newest terminated
// state. nil when there is no pod or no report yet.
func toolchainReport(p *corev1.Pod) *v1.AgentToolchainStatus {
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.Name != toolchainContainer {
			continue
		}
		t := cs.State.Terminated
		if t == nil {
			t = cs.LastTerminationState.Terminated
		}
		if t == nil {
			return nil
		}
		var m struct{ Key, Hash, Reason, Message string }
		if err := json.Unmarshal([]byte(t.Message), &m); err != nil || m.Reason == "" {
			return &v1.AgentToolchainStatus{Reason: "StepFailed", Pod: p.Name,
				Message: clip(fmt.Sprintf("exit %d without a report: %s", t.ExitCode, t.Message), 500)}
		}
		hex64 := regexp.MustCompile(`^([0-9a-f]{64})?$`)
		if !hex64.MatchString(m.Key) || !hex64.MatchString(m.Hash) {
			return &v1.AgentToolchainStatus{Reason: "Invalid", Pod: p.Name, Message: "the report has no valid key or hash"}
		}
		return &v1.AgentToolchainStatus{Key: m.Key, Hash: m.Hash, Reason: clip(m.Reason, 40), Message: clip(m.Message, 500), Pod: p.Name}
	}
	return nil
}

// observeToolchain reads the report of the agent pod, records a manifest mismatch as a security event, and keeps the
// last good "<key>:<hash>" in the ConfigMap agentdev-toolchains for the next start. Returns the status to show.
func (r *AgentReconciler) observeToolchain(ctx context.Context, a *v1.Agent) (*v1.AgentToolchainStatus, error) {
	if a.Spec.Toolchain == nil {
		return nil, nil
	}
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: "agent-" + a.Name + "-0"}, &pod)
	if apierrors.IsNotFound(err) {
		return a.Status.Toolchain, nil
	}
	if err != nil {
		return a.Status.Toolchain, err
	}
	rep := toolchainReport(&pod)
	if rep == nil {
		return a.Status.Toolchain, nil
	}
	if rep.Reason == "Reinstalled" {
		if err := r.toolchainEvent(ctx, a, &pod, rep); err != nil {
			return rep, err
		}
	}
	switch rep.Reason {
	case "Installed", "Verified", "Reinstalled":
		if err := r.writeToolchainHash(ctx, a.Namespace, a.Name, rep.Key+":"+rep.Hash); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// toolchainEvent: a changed toolchain manifest (ADR 0010 D3) is a high-severity security event. The report comes from
// the init container (agentdev's own script, before the agent ran), so it is not marked untrusted.
func (r *AgentReconciler) toolchainEvent(ctx context.Context, a *v1.Agent, p *corev1.Pod, rep *v1.AgentToolchainStatus) error {
	h := sha256.Sum256([]byte(string(p.UID) + "\x00toolchain\x00" + rep.Key + "\x00" + rep.Hash))
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "toolchain", Rule: "toolchain-manifest", Severity: "high",
		Agent: a.Name, Action: clip("the toolchain changed since the last start and was installed again: "+rep.Message, 500),
		Time: metav1.NewTime(r.now()), Pod: p.Name, Container: toolchainContainer, Hash: hex.EncodeToString(h[:])}
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: a.Namespace,
		Labels: map[string]string{"agent": a.Name, "agentdev/severity": spec.Severity, "agentdev/pod-uid": string(p.UID), "agentdev/job": ""}},
		Spec: spec}
	if err := r.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("toolchain security event: %w", err)
	}
	log.FromContext(ctx).Info("security event", "agent", a.Name, "rule", spec.Rule, "severity", spec.Severity)
	return nil
}

// writeToolchainHash keeps "<key>:<hash>" of an agent in agentdev-toolchains (one key per agent, like the guides).
func (r *AgentReconciler) writeToolchainHash(ctx context.Context, ns, agent, v string) error {
	var cm corev1.ConfigMap
	rd := r.Direct
	if rd == nil {
		rd = r.Client
	}
	err := rd.Get(ctx, types.NamespacedName{Namespace: ns, Name: toolchainsCM}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: toolchainsCM, Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/part-of": "agentdev"}}, Data: map[string]string{agent: v}}
		return r.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	if cm.Data[agent] == v {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[agent] = v
	return r.Update(ctx, &cm)
}
