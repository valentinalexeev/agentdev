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
	// Claude model of the agent's runs (`claude --model`, #564): an alias such as "opus" or "sonnet", or a full model
	// name such as "claude-opus-5-5". Unset: the Claude Code CLI's own default for the credential, as before.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:@\[\]-]*$`
	Model string `json:"model,omitempty"`
	// Reasoning effort of the agent's runs (`claude --effort`, #564). Unset: the CLI's own default, as before.
	// +optional
	// +kubebuilder:validation:Enum=low;medium;high;xhigh;max
	Effort string `json:"effort,omitempty"`
	// How Claude Code decides a tool call outside the allow-list (#500). Unset: dontAsk (refused, or asked through
	// approvals).
	// +optional
	Permission *AgentPermission `json:"permission,omitempty"`
	// Tools added to the worker's default allow-list for this agent (#496, ADR 0010 D1). Unset: the default only.
	// +optional
	Tools *AgentTools `json:"tools,omitempty"`
	// Toolchains installed before the agent starts (#499, ADR 0010 D2/D3): presets of agentdev with pinned checksums,
	// read-only to the agent. Unset: none, and the pod is as before.
	// +optional
	Toolchain *AgentToolchain `json:"toolchain,omitempty"`
	// Whether the agent may merge its own pull requests (#578). Unset or mode off: never, as before (a person merges).
	// +optional
	Merge *AgentMerge `json:"merge,omitempty"`
	// Stop when idle, woken by the router (#35). The router then owns replicas; resuming a paused agent starts it once.
	// Default true (#181): an Agent without the field scales to zero; set false to keep it running.
	// +kubebuilder:default=true
	// +optional
	ScaleToZero *bool `json:"scaleToZero,omitempty"`
	// Paused: no pod, state kept (day-2 pause/resume, #4). Unlike agentdev/blocked it is the owner's desired state.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// AgentToolchain is the agent's toolchain (#499, ADR 0010 D2). Only the administrator sets it (the Agent object).
// The init container toolchain installs it into /state/toolchain/<key> on the agent's volume; the agent container mounts
// /state/toolchain read-only. Free-form steps come later (ADR 0010 D4 step 3).
type AgentToolchain struct {
	// Preset versions. The operator accepts only the versions of its catalogue (a changed catalogue is an agentdev
	// release); another version makes the Agent invalid.
	Presets ToolchainPresets `json:"presets"`
	// PATH entries for the agent container, below $TOOLCHAIN, for example "$TOOLCHAIN/bin"; no segment starts with a
	// dot. The presets add their own.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:Pattern=`^\$TOOLCHAIN(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`
	Path []string `json:"path,omitempty"`
	// Install again after this time, for example 168h (at least 1h). Each agent has its own offset, and the new install
	// waits until the agent is idle (drain). Unset: only when the spec, the worker image or the catalogue changes.
	// +optional
	Refresh *metav1.Duration `json:"refresh,omitempty"`
}

// ToolchainPresets: one version per preset (#499). An empty version: the preset is not installed.
// +kubebuilder:validation:XValidation:rule="has(self.go) || has(self.rust)",message="spec.toolchain.presets needs at least one preset"
type ToolchainPresets struct {
	// Go, for example "1.27.4" (go.dev archive, sha256 pinned in the operator).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+(\.[0-9]+)?$`
	Go string `json:"go,omitempty"`
	// Rust, for example "1.100.0" (rustup with this toolchain, profile minimal; rustup-init sha256 pinned).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	Rust string `json:"rust,omitempty"`
}

// AgentToolchainStatus is the last report of the toolchain init container (#499), read from its termination message.
type AgentToolchainStatus struct {
	// SHA-256 of the canonical toolchain spec and the architecture: the directory /state/toolchain/<key>.
	// +optional
	Key string `json:"key,omitempty"`
	// SHA-256 manifest of the installed directory (every path, mode and content hash); checked on the next start.
	// +optional
	Hash string `json:"hash,omitempty"`
	// Installed, Verified, Reinstalled (the manifest had changed: a security event), StepFailed, Invalid.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Pod that reported it.
	// +optional
	Pod string `json:"pod,omitempty"`
}

// AgentTools holds the per-agent tool allow-list (#496, ADR 0010 D1). Only the administrator sets it (the Agent object),
// never the repository or issue text. It only adds to the default list: there is no replace, so a list cannot be
// shrunk to nothing or quietly swapped. The deny-list of the worker stays. The pod stays the boundary (threat model V6).
type AgentTools struct {
	// Claude Code permission rules added to the allow-list, for example "Bash(npm ci:*)". One rule per entry, no
	// commas and no control characters.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f,]+$`
	Allow []string `json:"allow,omitempty"`
}

// AgentMerge lets the agent merge its own pull request once a person approved its exact head commit (#578). Only the
// administrator sets it (the Agent object), never the repository or issue text. The forge sidecar checks the approval,
// the checks and the protected paths itself, from the forge, at the moment of the merge (images/forge/merge.go).
type AgentMerge struct {
	// off: the agent never merges (default). gated: it merges its own PR (head agent/<name>/*) when a person with
	// write access approved the head commit, every check passed and no change is requested. GitHub only.
	// +kubebuilder:validation:Enum=off;gated
	// +kubebuilder:default=off
	Mode string `json:"mode"`
	// Merge method, as the repository allows it. Default merge.
	// +kubebuilder:validation:Enum=merge;squash;rebase
	// +optional
	Method string `json:"method,omitempty"`
}

// AgentPermission is the Claude Code permission mode of the agent (#500). Only the administrator sets it (the Agent
// object); never the repository or issue text. The classifier of mode auto is a model's judgment, not a boundary: the
// pod stays the boundary (threat model V6). bypassPermissions is not supported.
// +kubebuilder:validation:XValidation:rule="!has(self.autoMode) || (has(self.mode) && self.mode == 'auto')",message="spec.permission.autoMode needs mode auto"
type AgentPermission struct {
	// dontAsk (default): a call outside the allow-list is refused, or asked through approvals. auto: Claude Code's
	// classifier decides it; a call the classifier blocks is refused, or asked through approvals.
	// +kubebuilder:validation:Enum=dontAsk;auto
	// +kubebuilder:default=dontAsk
	Mode string `json:"mode,omitempty"`
	// Rules for the classifier of mode auto, the autoMode section of the Claude Code settings. Unset: Claude Code's
	// defaults (`claude auto-mode defaults`).
	// +optional
	AutoMode *AgentAutoMode `json:"autoMode,omitempty"`
}

// AgentAutoMode holds the classifier rules (#500), in prose, one rule per entry. The hard deny rules stay Claude Code's
// own: an Agent cannot change them. No control characters: one rule is one line.
type AgentAutoMode struct {
	// Actions the classifier allows (autoMode.allow).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=500
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f]+$`
	Allow []string `json:"allow,omitempty"`
	// Actions the classifier blocks unless the task clearly asks for them (autoMode.soft_deny).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=500
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f]+$`
	SoftDeny []string `json:"softDeny,omitempty"`
	// Facts about the environment for the classifier, for example trusted repositories (autoMode.environment).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=500
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f]+$`
	Environment []string `json:"environment,omitempty"`
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
	// The toolchain init container's last report (#499); unset without spec.toolchain.
	// +optional
	Toolchain *AgentToolchainStatus `json:"toolchain,omitempty"`
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
