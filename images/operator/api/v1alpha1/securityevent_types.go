package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaV1 is the version of the security event schema (#70). It is the same for the log line, the SecurityEvent
// object, the security_events table and the webhook body; field meanings are in docs/security-model.md.
const SchemaV1 = "agentdev.security/v1"

// SecurityEventSpec is one guardrail refusal (#64, #70). Written by the operator from the agent pod's log line; never
// by the agent (agents have no RBAC on this resource). Immutable once created.
type SecurityEventSpec struct {
	// Schema version, agentdev.security/v1.
	Schema string `json:"schema"`
	// Component that refused: forge (sidecar), loop (agent loop); later egress, claude-proxy, approvals.
	// +kubebuilder:validation:MaxLength=40
	Source string `json:"source"`
	// Rule that fired, for example push-branch or git-config.
	// +kubebuilder:validation:MaxLength=80
	Rule string `json:"rule"`
	// +kubebuilder:validation:Enum=low;medium;high
	Severity string `json:"severity"`
	// Agent name: taken from the pod's agent label, not from the log line.
	Agent string `json:"agent"`
	// Job the agent worked on, issue-N or pr-N; empty when not known (a missing and an empty job are the same).
	// +optional
	Job string `json:"job,omitempty"`
	// The refused action as text, at most 500 characters plus an ellipsis.
	// +kubebuilder:validation:MaxLength=520
	Action string `json:"action"`
	// When the line was written (container runtime timestamp).
	Time metav1.Time `json:"time"`
	// Pod and container that wrote the line.
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// SHA-256 over pod UID, container, timestamp and the raw line: the dedupe key; the object name is derived from it.
	Hash string `json:"hash"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=sev
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.severity`
// +kubebuilder:printcolumn:name="Rule",type=string,JSONPath=`.spec.rule`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.job`
// +kubebuilder:printcolumn:name="Time",type=date,JSONPath=`.spec.time`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="SecurityEvent is immutable"
type SecurityEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SecurityEventSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type SecurityEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecurityEvent `json:"items"`
}

func init() { SchemeBuilder.Register(&SecurityEvent{}, &SecurityEventList{}) }
