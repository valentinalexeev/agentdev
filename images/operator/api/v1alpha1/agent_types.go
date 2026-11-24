// Package v1alpha1: the Agent resource (#47, decision A on the issue). An Agent is one coding agent: a forge account
// working one repository. The operator turns it into the agent's Service and StatefulSet (render.go) (Service,
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
	// Turn limit of one Claude run (`claude -p --max-turns`, #336). A larger task may need more. 0 or unset: 40.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	// +optional
	MaxTurns int32 `json:"maxTurns,omitempty"`
	// Claude Code tool allow-list of the agent (#496). Unset: the worker's default list.
	// +optional
	Tools *AgentTools `json:"tools,omitempty"`
	// Stop when idle, woken by the router (#35). The router then owns replicas; resuming a paused agent starts it once.
	// Default true (#181): an Agent without the field scales to zero; set false to keep it running.
	// +kubebuilder:default=true
	// +optional
	ScaleToZero *bool `json:"scaleToZero,omitempty"`
	// Paused: no pod, state kept (day-2 pause/resume, #4). Unlike agentdev/blocked it is the owner's desired state.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// AgentTools changes the tool allow-list Claude Code gets (`--allowedTools`, #496). Each entry is one tool in Claude
// Code's syntax, such as Read or Bash(npm ci:*). No commas or control characters: the worker joins the list with commas
// and also shows it in the system prompt. Widening the list widens what a prompt injection can run (threat model V6).
type AgentTools struct {
	// Tools added to the default list (or to replace), for example Bash(npm ci:*).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z][A-Za-z0-9_-]*(\([^,()\x00-\x1f]+\))?$`
	Allow []string `json:"allow,omitempty"`
	// Replaces the worker's default list. Unset or empty: the default list.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z][A-Za-z0-9_-]*(\([^,()\x00-\x1f]+\))?$`
	Replace []string `json:"replace,omitempty"`
}

// AgentStatus is observed state, written by the operator only.
type AgentStatus struct {
	// running | starting | stopped | paused | blocked
	Phase string `json:"phase,omitempty"`
	// Ready pods (0 or 1).
	Ready int32 `json:"ready,omitempty"`
	// Worker image of the StatefulSet: the version the agent runs at its next wake. A stopped agent keeps running
	// the old pod image until then.
	// +optional
	Image string `json:"image,omitempty"`
	// .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Reconciled, SecurityPaused and Ready. Ready is True for running, starting, stopped and paused (the router can
	// wake the agent) and False for invalid and blocked (#386); Flux and Argo CD health checks read it.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// Name becomes agent-<name> (StatefulSet, Service) and state-agent-<name>-0 (PVC): keep it a short DNS label.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="agent name must be at most 40 characters"
// Secrets are bound to the agent: its own token Secret only, and its own Claude Secret or the shared one. Another
// agent's credentials can never be named, whoever may write Agents.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.forgeSecret) || self.spec.forgeSecret == 'agent-' + self.metadata.name",message="spec.forgeSecret must be agent-<agent name>"
// +kubebuilder:validation:XValidation:rule="self.spec.claudeSecret == 'claude-shared' || self.spec.claudeSecret == 'claude-' + self.metadata.name",message="spec.claudeSecret must be claude-shared or claude-<agent name>"
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

// ScalesToZero reports whether the agent stops when idle: true unless scaleToZero is explicitly false (#181). The CRD
// default sets the field on new objects; this covers objects read without it (tests, older API servers).
func (s AgentSpec) ScalesToZero() bool { return s.ScaleToZero == nil || *s.ScaleToZero }
