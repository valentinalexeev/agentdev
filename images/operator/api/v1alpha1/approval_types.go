package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApprovalSpec is one permission request of an agent (#67). Written by the operator from the forge sidecar's
// AGENTDEV_APPROVAL_REQUEST log line (never from the agent container, never by the agent: agents have no RBAC on this
// resource). Immutable once created; the decision is in the status, written only by agentdev-api.
type ApprovalSpec struct {
	// Request id: a random nonce chosen by the sidecar. Decisions quote it (/approve <id>).
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{16}$`
	ID string `json:"id"`
	// Agent name: taken from the pod's agent label, not from the log line.
	// +kubebuilder:validation:MaxLength=63
	Agent string `json:"agent"`
	// Repository (owner/name) of the agent: the project. Anyone with write access to it may decide.
	// +kubebuilder:validation:MaxLength=201
	Repo string `json:"repo"`
	// Forge kind of the repository.
	// +kubebuilder:validation:Enum=gitea;github
	Forge string `json:"forge"`
	// Job the request belongs to, issue-N or pr-N. The forge comments are read on that issue or PR.
	// +kubebuilder:validation:Pattern=`^(issue|pr)-[0-9]{1,10}$`
	Job string `json:"job"`
	// What asks: claude (a tool permission prompt) or push (a push touching a protected path).
	// +kubebuilder:validation:Enum=claude;push
	Source string `json:"source"`
	// Tool name (claude) or "push" (push).
	// +kubebuilder:validation:MaxLength=128
	Tool string `json:"tool"`
	// The request as text: the tool input, or the branch, commit and protected paths. Untrusted: written by Claude's
	// choice of input, shown to people as data only.
	// +kubebuilder:validation:MaxLength=4100
	Input string `json:"input"`
	// SHA-256 (hex) over id, agent, repo, job, source, tool and input, computed by the sidecar. A decision covers this
	// hash; the sidecar acts only on a decision with the hash it computed.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	ContentHash string `json:"contentHash"`
	// When the sidecar asked (container runtime timestamp of the line).
	Created metav1.Time `json:"created"`
	// After this time the request expires (same effect as a denial).
	Expires metav1.Time `json:"expires"`
}

// ApprovalStatus is written by agentdev-api only, with a compare-and-set on resourceVersion: the first valid decision
// wins.
type ApprovalStatus struct {
	// pending (empty) | allowed | denied | expired
	// +optional
	// +kubebuilder:validation:Enum="";pending;allowed;denied;expired
	State string `json:"state,omitempty"`
	// once | job: for allowed, how far the approval reaches (never beyond the job).
	// +optional
	Scope string `json:"scope,omitempty"`
	// Forge login of the person who decided, and where (forge).
	// +optional
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecidedVia string `json:"decidedVia,omitempty"`
	// Forge comment id of the decision.
	// +optional
	CommentID int64 `json:"commentId,omitempty"`
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
	// The content hash the decision covers (copied from spec when deciding).
	// +optional
	ContentHash string `json:"contentHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=apv
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.job`
// +kubebuilder:printcolumn:name="Tool",type=string,JSONPath=`.spec.tool`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="By",type=string,JSONPath=`.status.decidedBy`
// +kubebuilder:printcolumn:name="Created",type=date,JSONPath=`.spec.created`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="Approval spec is immutable"
type Approval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApprovalSpec   `json:"spec"`
	Status            ApprovalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Approval `json:"items"`
}

func init() { SchemeBuilder.Register(&Approval{}, &ApprovalList{}) }
