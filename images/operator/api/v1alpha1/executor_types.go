package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Deterministic executors (#69, docs/security-model.md phase 5). An Executor runs one fixed tool (for example
// terraform) for one environment on one repository: a plan of every pull request that touches its path, a plan of
// every merge commit, and, after a person approved that plan (Approval, #67), an apply of exactly that plan. No AI
// runs in it and it takes no input from the repository except the commit to check out: image and commands are fixed
// here, and only cluster admins may write Executors (the operator's own namespace, no agent RBAC).

// ExecutorCommand is a fixed argv. Kubernetes expands $(PLAN_FILE), $(COMMIT) and $(ENVIRONMENT) in it; nothing else
// is substituted and no shell is started unless the command itself is one.
type ExecutorCommand struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=1024
	Command []string `json:"command"`
}

// ExecutorCredentials are what one phase (plan or apply) of the tool runs with. The Secret's keys become the tool
// container's environment; the service account carries the cloud identity (GKE Workload Identity annotation). The
// Kubernetes API token is never mounted.
type ExecutorCredentials struct {
	// Secret exec-<executor>-plan (plan) or exec-<executor>-apply (apply). Optional.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// Kubernetes service account of the Job, exec-*. Optional.
	// +optional
	// +kubebuilder:validation:Pattern=`^exec-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// ExecutorSpec is written by cluster admins only.
type ExecutorSpec struct {
	// Environment this executor changes, for example prod. One executor per environment (owner decision on #69):
	// the operator marks every later Executor of the same environment invalid.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=30
	Environment string `json:"environment"`
	// Repository, owner/name.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	// +kubebuilder:validation:MaxLength=201
	Repo string `json:"repo"`
	// +kubebuilder:validation:Enum=gitea;github
	// +kubebuilder:default=gitea
	Forge string `json:"forge,omitempty"`
	// Protected branch whose merge commits may be applied.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:default=main
	Branch string `json:"branch,omitempty"`
	// Directory in the repository the tool runs in. A pull request is planned only if it changes a file below it.
	// Empty: the repository root and every pull request.
	// +optional
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9_-][A-Za-z0-9_.-]*/)*[A-Za-z0-9_-][A-Za-z0-9_.-]*$`
	// +kubebuilder:validation:MaxLength=200
	Path string `json:"path,omitempty"`
	// Tool image, pinned by digest (name@sha256:...). Never taken from the repository.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9./_:-]*@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=300
	Image string `json:"image"`
	// Plan command: must write the plan to $(PLAN_FILE).
	Plan ExecutorCommand `json:"plan"`
	// Apply command: must apply exactly $(PLAN_FILE).
	Apply ExecutorCommand `json:"apply"`
	// Read-only credentials, used for every plan (also of unreviewed pull requests).
	// +optional
	PlanCredentials ExecutorCredentials `json:"planCredentials,omitempty"`
	// Write credentials, used only by the apply Job of an approved plan.
	// +optional
	ApplyCredentials ExecutorCredentials `json:"applyCredentials,omitempty"`
	// Secret exec-<executor>-forge with FORGE_TOKEN of the executor's own forge account (read code, set the commit
	// status, comment). This account can never decide an approval (#67: requests of an account are refused from it).
	ForgeSecret string `json:"forgeSecret"`
	// Hours an apply approval stays open.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=168
	// +kubebuilder:default=24
	// +optional
	ApprovalHours int32 `json:"approvalHours,omitempty"`
	// Paused: no new runs are started; running Jobs finish.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// AllowForkPRs: also plan pull requests whose head is in another repository (a fork). Off by default: a fork PR
	// runs code of anyone who can open a PR with the read-only plan credentials (docs/security.md R81, egress #65).
	// +optional
	AllowForkPRs bool `json:"allowForkPRs,omitempty"`
	// EgressHosts: the hosts the Job may reach through the executor egress proxy (#135), in addition to the
	// cluster-wide defaults (EXECUTOR_EGRESS_HOSTS) and the forge's git host. Squid dstdomain form: "example.com" is
	// that name only, ".example.com" the domain and every subdomain. Every other connection is refused and recorded
	// as a SecurityEvent (source egress). Read by every run; not part of the approved spec hash.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^\.?([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:items:MaxLength=253
	EgressHosts []string `json:"egressHosts,omitempty"`
}

// ExecutorStatus is written by the operator only.
type ExecutorStatus struct {
	// ready | paused | invalid | error
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastPoll *metav1.Time `json:"lastPoll,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=exe
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repo`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// The name becomes part of Job names (<name>-merge-<12 hex>): keep it short.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="executor name must be at most 40 characters"
// Credentials are bound to the executor and to the phase: plan and apply never share a Secret or a service account.
// +kubebuilder:validation:XValidation:rule="self.spec.forgeSecret == 'exec-' + self.metadata.name + '-forge'",message="spec.forgeSecret must be exec-<name>-forge"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.planCredentials) || !has(self.spec.planCredentials.secretName) || self.spec.planCredentials.secretName == 'exec-' + self.metadata.name + '-plan'",message="spec.planCredentials.secretName must be exec-<name>-plan"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.applyCredentials) || !has(self.spec.applyCredentials.secretName) || self.spec.applyCredentials.secretName == 'exec-' + self.metadata.name + '-apply'",message="spec.applyCredentials.secretName must be exec-<name>-apply"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.planCredentials) || !has(self.spec.planCredentials.serviceAccountName) || !has(self.spec.applyCredentials) || !has(self.spec.applyCredentials.serviceAccountName) || self.spec.planCredentials.serviceAccountName != self.spec.applyCredentials.serviceAccountName",message="plan and apply must use different service accounts"
type Executor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExecutorSpec   `json:"spec"`
	Status            ExecutorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ExecutorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Executor `json:"items"`
}

// ExecutorRunSpec is one action on one commit. Created by the operator only; immutable.
type ExecutorRunSpec struct {
	// Executor name.
	// +kubebuilder:validation:MaxLength=40
	Executor string `json:"executor"`
	// plan | apply
	// +kubebuilder:validation:Enum=plan;apply
	Action string `json:"action"`
	// pr: plan of a pull request head (unreviewed code, result informative only). merge: plan of a merge commit of
	// the protected branch (the only plan that can be applied). apply: apply of an approved merge plan.
	// +kubebuilder:validation:Enum=pr;merge;apply
	Stage string `json:"stage"`
	// Commit, 40 hex characters (from the forge API, checked by the operator).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`
	// Pull request number the results are posted to.
	// +kubebuilder:validation:Minimum=1
	PR int64 `json:"pr"`
	// apply: the Approval that allowed it.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Approval string `json:"approval,omitempty"`
	// apply: SHA-256 of the approved plan file; the Job refuses to apply any other file.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{64})?$`
	PlanHash string `json:"planHash,omitempty"`
	// apply: SHA-256 of the Executor spec (image digest, commands, credentials, repo, branch, path) the merge plan ran
	// with and the approval covers; the apply is refused when the Executor differs.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{64})?$`
	SpecHash string `json:"specHash,omitempty"`
	// apply: the Job is not started after this time (decision time plus EXECUTOR_APPLY_START_MINUTES).
	// +optional
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
}

// ExecutorRunStatus is written by the operator only.
type ExecutorRunStatus struct {
	// Pending | Running | Succeeded | Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// plan: SHA-256 of the plan file, computed by the operator's own image in the Job (not by the tool).
	// +optional
	PlanHash string `json:"planHash,omitempty"`
	// merge plan: SHA-256 of the Executor spec the plan ran with (see ExecutorRunSpec.SpecHash).
	// +optional
	SpecHash string `json:"specHash,omitempty"`
	// merge plan: name of the Approval requested for the apply.
	// +optional
	Approval string `json:"approval,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// The result is posted to the forge.
	// +optional
	Reported bool `json:"reported,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=exr
// +kubebuilder:printcolumn:name="Executor",type=string,JSONPath=`.spec.executor`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.spec.pr`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.spec.commit`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="ExecutorRun spec is immutable"
type ExecutorRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExecutorRunSpec   `json:"spec"`
	Status            ExecutorRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ExecutorRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutorRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Executor{}, &ExecutorList{}, &ExecutorRun{}, &ExecutorRunList{})
}
