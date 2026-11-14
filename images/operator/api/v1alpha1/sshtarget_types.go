package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SSH server access for executors (#204, docs/adr/0007). An SSHTarget is one server that one Executor may reach with
// short-lived SSH certificates from the instance CA (step-ca in EXEC_NS). Written by cluster admins only; the
// operator signs the server's host key only when it matches spec.hostKeyFingerprint, and sets Ready only after a
// probe with a certificate succeeded against the signed host key.

// SSHTargetSpec is written by cluster admins only.
type SSHTargetSpec struct {
	// Host name or IP address the operator and the executor Jobs connect to. Also the host certificate's principal.
	// +kubebuilder:validation:Pattern=`^(([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)*[a-z0-9]([-a-z0-9]*[a-z0-9])?|[0-9]{1,3}(\.[0-9]{1,3}){3})$`
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=22
	// +optional
	Port int32 `json:"port,omitempty"`
	// Executor that may use this server (merge plan with the check principal, apply with the apply principal).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Executor string `json:"executor"`
	// Local user of the apply stage (principal agentdev-<executor>-apply; the only user with sudo).
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_-]{0,31}$`
	// +kubebuilder:default=agentdev
	// +optional
	ApplyUser string `json:"applyUser,omitempty"`
	// Local user of the check stage and the probe (principals agentdev-<executor>-check and agentdev-probe; no sudo).
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_-]{0,31}$`
	// +kubebuilder:default=agentdev-check
	// +optional
	CheckUser string `json:"checkUser,omitempty"`
	// SHA-256 fingerprint of the server's host key as `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` prints it
	// (SHA256:...), read on the server by a person. The operator signs the host key only if it matches: this is the
	// admin's decision that the server is the right one.
	// +kubebuilder:validation:Pattern=`^SHA256:[A-Za-z0-9+/]{43}$`
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
}

// SSHTargetStatus is written by the operator only.
type SSHTargetStatus struct {
	// Pending | HostKeyMismatch | Enrolling | Ready | NotReady
	// +optional
	Phase string `json:"phase,omitempty"`
	// Fingerprint of the host key the server presented at the last connection.
	// +optional
	ObservedFingerprint string `json:"observedFingerprint,omitempty"`
	// Host certificate (OpenSSH authorized_keys form) for /etc/ssh/ssh_host_ed25519_key-cert.pub.
	// +optional
	HostCertificate string `json:"hostCertificate,omitempty"`
	// +optional
	HostCertificateNotAfter *metav1.Time `json:"hostCertificateNotAfter,omitempty"`
	// User CA public keys for TrustedUserCAKeys (several during a rotation).
	// +optional
	UserCAKeys []string `json:"userCAKeys,omitempty"`
	// Host CA public key, for the @cert-authority line of the executors.
	// +optional
	HostCAKey string `json:"hostCAKey,omitempty"`
	// +optional
	LastProbe *metav1.Time `json:"lastProbe,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sst
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Executor",type=string,JSONPath=`.spec.executor`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Probe",type=date,JSONPath=`.status.lastProbe`
// +kubebuilder:validation:XValidation:rule="!has(self.spec.applyUser) || !has(self.spec.checkUser) || self.spec.applyUser != self.spec.checkUser",message="applyUser and checkUser must differ"
type SSHTarget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SSHTargetSpec   `json:"spec"`
	Status            SSHTargetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SSHTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHTarget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SSHTarget{}, &SSHTargetList{})
}
