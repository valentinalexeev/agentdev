package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NotificationChannel is an instance-level notification channel (#347, owner decision on #347: the instance part is
// a resource like ModelConfig and McpServer; the per-user part, subscriptions and mutes, stays in the API database).
// It lives in the platform namespace next to the API, which reads it; only cluster admins (Ansible) write it. The
// API uses the first valid channel of a type (by name) when the instance value notifications.channels turns the type on.
//
// Type webpush: the VAPID key pair is in the Secret spec.vapid.secretRef (keys publicKey and privateKey, base64url,
// created once by Ansible); allowedHosts are the push services the API may send to (subscription endpoints on other
// hosts are refused) and the hosts of the API egress policy. Type bark follows with #402.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'webpush' || has(self.vapid)",message="type webpush needs spec.vapid"
// +kubebuilder:validation:XValidation:rule="self.type != 'webpush' || (has(self.subject) && (self.subject.startsWith('mailto:') || self.subject.startsWith('https://')))",message="type webpush needs spec.subject mailto: or https://"
type NotificationChannelSpec struct {
	// +kubebuilder:validation:Enum=webpush
	Type string `json:"type"`
	// Contact of the instance for the push services (VAPID "sub", RFC 8292): mailto: or https://.
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Subject string `json:"subject,omitempty"`
	// VAPID key pair of type webpush.
	// +optional
	VAPID *NotificationVAPID `json:"vapid,omitempty"`
	// Hosts the API may send to: exact names, or *.suffix for any name below suffix. Empty: the default list of the
	// API (fcm.googleapis.com, updates.push.services.mozilla.com, *.push.apple.com, *.notify.windows.com).
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

// NotificationVAPID references the Secret with the VAPID key pair.
type NotificationVAPID struct {
	SecretRef NotificationSecretRef `json:"secretRef"`
}

// NotificationSecretRef is a Secret in the namespace of the NotificationChannel.
type NotificationSecretRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=nch
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`

// NotificationChannel is a notification channel of the instance (#347).
type NotificationChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NotificationChannelSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// NotificationChannelList is a list of NotificationChannel.
type NotificationChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NotificationChannel `json:"items"`
}

func init() { SchemeBuilder.Register(&NotificationChannel{}, &NotificationChannelList{}) }
