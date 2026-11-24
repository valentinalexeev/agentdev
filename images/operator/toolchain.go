package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// toolchainSetup runs in the worker image's python3 (#499); embedded, so it needs no change of the worker image.
//
//go:embed toolchain_setup.py
var toolchainSetup string

const (
	toolchainDir = "/state/toolchain"
	// PATH of the worker image (node:24-bookworm-slim): the env var replaces it, so the toolchain path goes in front
	imagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// toolchainContainer is the setup phase of an agent with spec.toolchain (#499): an init container of the worker image,
// after state-init, that installs the tools into /state/toolchain. It has the state volume and the egress proxy only:
// no forge or Claude credential, no proxy token, no service account token. nil without a toolchain.
func (r *AgentReconciler) toolchainContainer(a *v1.Agent, image string) *corev1.Container {
	tc := a.Spec.Toolchain
	if tc == nil || len(tc.Steps) == 0 {
		return nil
	}
	spec, _ := json.Marshal(tc) // plain strings and numbers: cannot fail
	refresh := int64(0)
	if d, err := time.ParseDuration(tc.Refresh); err == nil && d > 0 {
		refresh = int64(d / time.Second)
	}
	env := append([]corev1.EnvVar{{Name: "TOOLCHAIN_SPEC", Value: string(spec)}, {Name: "TOOLCHAIN_DIR", Value: toolchainDir},
		{Name: "TOOLCHAIN_REFRESH_SECONDS", Value: fmt.Sprint(refresh)}}, r.proxyEnv(false)...)
	return &corev1.Container{
		Name: "toolchain", Image: image, ImagePullPolicy: corev1.PullPolicy(r.D.PullPolicy),
		TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Command:      []string{"python3", "-I", "-c", toolchainSetup},
		Env:          env,
		VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: "/state"}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")}},
	}
}

// toolchainPath is the agent container's PATH with the toolchain directories in front; "" without a toolchain path,
// so the image's PATH stays (and the pod template of an agent without toolchain does not change).
func toolchainPath(a *v1.Agent) string {
	if a.Spec.Toolchain == nil || len(a.Spec.Toolchain.Path) == 0 {
		return ""
	}
	return strings.Join(append(append([]string{}, a.Spec.Toolchain.Path...), imagePath), ":")
}
