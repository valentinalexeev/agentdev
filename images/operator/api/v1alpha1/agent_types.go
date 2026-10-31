// Package v1alpha1: the Agent resource (#47, decision A on the issue). An Agent is one coding agent: a forge account
// working one repository. The operator turns it into the objects manifests/agent.yaml.tmpl describes (Service,
// StatefulSet); secrets (forge token, Claude credential) stay outside the spec and are referenced by name.
// +kubebuilder:object:generate=true
// +groupName=agentdev.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "agentdev.dev", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// AgentSpec is the desired agent. Defaults match 06-agents.sh.
type AgentSpec struct {
	// Repository the agent works on, owner/name.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repo string `json:"repo"`
	// Forge kind.
	// +kubebuilder:validation:Enum=gitea;github
	// +kubebuilder:default=gitea
	Forge string `json:"forge,omitempty"`
	// Secret with FORGE_TOKEN (and optional WAKE_TOKEN, REGISTER_TOKEN). Default agent-<name>. Only agent-* names:
	// whoever may write Agents must not be able to mount arbitrary Secrets of the namespace into a pod.
	// +optional
	// +kubebuilder:validation:Pattern=`^agent-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ForgeSecret string `json:"forgeSecret,omitempty"`
	// Secret with the Claude credential (ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN). Exactly one per agent (#5).
	// Only claude-* names (claude-shared or claude-<agent>), for the same reason.
	// +kubebuilder:validation:Pattern=`^claude-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ClaudeSecret string `json:"claudeSecret"`
	// No image field on purpose: the worker image is the operator's (WORKER_IMAGE), so writing an Agent cannot run
	// arbitrary code with the agent's credentials.
	// Parallel items (#20). Memory limit is (1 + maxConcurrent) Gi.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +kubebuilder:default=2
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
	// Stop when idle, woken by the router (#35). The router then owns replicas; resuming a paused agent starts it once.
	// +optional
	ScaleToZero bool `json:"scaleToZero,omitempty"`
	// Paused: no pod, state kept (day-2 pause/resume, #4). Unlike agentdev/blocked it is the owner's desired state.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// AgentStatus is observed state, written by the operator only.
type AgentStatus struct {
	// running | starting | stopped | paused | blocked
	Phase string `json:"phase,omitempty"`
	// Ready pods (0 or 1).
	Ready int32 `json:"ready,omitempty"`
	// .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// Name becomes agent-<name> (StatefulSet, Service) and state-agent-<name>-0 (PVC): keep it a short DNS label.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="agent name must be at most 40 characters"
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSpec   `json:"spec,omitempty"`
	Status            AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() { SchemeBuilder.Register(&Agent{}, &AgentList{}) }
