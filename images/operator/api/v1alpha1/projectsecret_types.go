package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Scoped secrets store (#202, ADR 0003 D1/D2). A ProjectSecret says who may use a credential and how it is delivered;
// it never holds the value. The value lives in the instance secret backend (GCP Secret Manager on GKE, a Kubernetes
// Secret in the store namespace on kind) under the remote key agentdev-ps-<name>, written only by the API (write-only:
// set, rotate, delete). The operator syncs it with External Secrets Operator into the Secret ps-<name> next to the
// broker; agent pods never mount it (mode broker).
//
// Scopes in the first release (owner decision on #202): project (every agent of the repository) and agent (one
// agent). user follows with #26; instance stays with the admin's Ansible Secrets.

// ProjectSecretBroker is mode broker: the credential broker (generalised credproxy) adds the credential to requests
// for one upstream. The agent calls the broker as the tool's base URL; it never sees the value.
type ProjectSecretBroker struct {
	// Upstream base URL the broker forwards to, https only, for example https://npm.pkg.github.com.
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`
	// +kubebuilder:validation:MaxLength=300
	Upstream string `json:"upstream"`
	// Request header that carries the credential.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9-]{0,63}$`
	// +kubebuilder:default=Authorization
	Header string `json:"header,omitempty"`
	// How the value is written into the header: bearer ("Bearer <value>"), basic ("Basic <value>", the value is
	// already user:password in base64) or raw (the value as is).
	// +kubebuilder:validation:Enum=bearer;basic;raw
	// +kubebuilder:default=bearer
	Scheme string `json:"scheme,omitempty"`
	// npm scope served by this upstream, for example @acme (private registries, #248). The operator then writes an
	// npm config for every agent in scope that gets @acme/* packages through the broker with the agent's key; other
	// packages keep the default registry (or the registry cache, #203). The upstream must be an npm registry.
	// +optional
	// +kubebuilder:validation:Pattern=`^@[a-z0-9][-a-z0-9._]{0,99}$`
	NpmScope string `json:"npmScope,omitempty"`
}

// ProjectSecretEnv is mode env: the value becomes an environment variable of the agent container, readable by the
// model. Explicit opt-in (owner decision on #202): riskAccepted must be true, the risk is recorded (docs/security.md),
// and the value is redacted from approval text.
type ProjectSecretEnv struct {
	// Variable name. AGENTDEV_*, ANTHROPIC_*, CLAUDE_*, FORGE_*, GIT_*, HTTP(S)_PROXY and the like are refused.
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Z0-9_]{0,63}$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(AGENTDEV|ANTHROPIC|CLAUDE|FORGE|GIT|GH|GITHUB|SSH|KUBE|KUBERNETES)_') && !(self in ['PATH','HOME','HTTP_PROXY','HTTPS_PROXY','NO_PROXY','LD_PRELOAD','LD_LIBRARY_PATH','NODE_OPTIONS','BASH_ENV','ENV'])",message="reserved variable name"
	Name string `json:"name"`
	// Must be true: the person who sets it accepts that the model can read the value.
	// +kubebuilder:validation:XValidation:rule="self == true",message="mode env needs riskAccepted: true"
	RiskAccepted bool `json:"riskAccepted"`
}

// ProjectSecretSpec is written by the API for project maintainers (or by admins). Scope, owner and mode never change:
// delete and add again.
// +kubebuilder:validation:XValidation:rule="self.scope != 'project' || (has(self.project) && !has(self.agent))",message="scope project needs spec.project and no spec.agent"
// +kubebuilder:validation:XValidation:rule="self.scope != 'agent' || (has(self.agent) && has(self.project))",message="scope agent needs spec.agent and spec.project (the agent's repository)"
// +kubebuilder:validation:XValidation:rule="self.mode != 'broker' || (has(self.broker) && !has(self.env))",message="mode broker needs spec.broker and no spec.env"
// +kubebuilder:validation:XValidation:rule="self.mode != 'env' || (has(self.env) && !has(self.broker))",message="mode env needs spec.env and no spec.broker"
// +kubebuilder:validation:XValidation:rule="self.scope == oldSelf.scope && self.mode == oldSelf.mode && (has(self.project) ? has(oldSelf.project) && self.project == oldSelf.project : !has(oldSelf.project)) && (has(self.agent) ? has(oldSelf.agent) && self.agent == oldSelf.agent : !has(oldSelf.agent)) && (has(self.forge) ? has(oldSelf.forge) && self.forge == oldSelf.forge : !has(oldSelf.forge))",message="scope, project, agent, forge and mode are immutable"
type ProjectSecretSpec struct {
	// +kubebuilder:validation:Enum=project;agent
	Scope string `json:"scope"`
	// Forge of spec.project. A repository is identified by (forge, owner/name): acme/app on in-cluster Gitea, where
	// anyone may create the org, is not acme/app on GitHub.
	// +kubebuilder:validation:Enum=gitea;github
	// +kubebuilder:default=gitea
	// +optional
	Forge string `json:"forge,omitempty"`
	// Repository whose agents may use it, owner/name. Scope project: every agent of it. Scope agent: the agent's
	// repository when the secret was created; the secret applies only while the agent still works on it, so an agent
	// recreated under the same name for another repository does not inherit it.
	// +optional
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	// +kubebuilder:validation:MaxLength=201
	Project string `json:"project,omitempty"`
	// Agent that may use it (scope agent).
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Agent string `json:"agent,omitempty"`
	// Delivery mode. identity, executor and ssh (ADR 0003 D2) store no value here: Workload Identity, the Executor's
	// exec-* Secrets and the SSH CA (#204).
	// +kubebuilder:validation:Enum=broker;env
	Mode string `json:"mode"`
	// +optional
	Broker *ProjectSecretBroker `json:"broker,omitempty"`
	// +optional
	Env *ProjectSecretEnv `json:"env,omitempty"`
	// Free text for people, shown in the UI. Never the value.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Description string `json:"description,omitempty"`
}

// ProjectSecretStatus is written by the operator.
type ProjectSecretStatus struct {
	// pending (no value yet or not synced), ready (synced into ps-<name>), error.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Reason for pending or error, from External Secrets Operator. Never contains the value.
	// +optional
	Message string `json:"message,omitempty"`
	// Name of the synced Secret, ps-<name>.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// When the value was last synced (External Secrets Operator's refresh time).
	// +optional
	SyncedAt *metav1.Time `json:"syncedAt,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=psec
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50",message="ProjectSecret name must be at most 50 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="ProjectSecret name must be a DNS label"
type ProjectSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSecretSpec   `json:"spec"`
	Status ProjectSecretStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ProjectSecretList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectSecret `json:"items"`
}

func init() { SchemeBuilder.Register(&ProjectSecret{}, &ProjectSecretList{}) }
