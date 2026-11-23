package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCP per project (#14, ADR 0001; operator controller since #383, was scripts/mcp-sync.sh). An McpProject names the
// MCP servers one project may use and the agents that get them. The operator registers each server as a ContextForge
// gateway <project>-<server>, keeps one virtual server project-<project> with exactly their tools, and one token per
// agent scoped to that virtual server in Secret agent-<agent>-mcp {CF_URL, CF_TOKEN}. metadata.name is the project.
//
// Which hosts may be registered is an instance setting (MCP_ALLOWED_HOSTS on the operator), not part of this spec:
// whoever writes an McpProject cannot widen it. A server on another host stops the reconcile before any change and
// the reason is in status (Ready=False, HostNotAllowed).

// McpServer is one MCP server of the project.
type McpServer struct {
	// Short name; the ContextForge gateway is <project>-<name>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`
	// Streamable HTTP endpoint, http(s)://host[:port]/path without credentials. ContextForge only calls the cluster
	// Service range (SSRF guard), and the host must be in the instance allow-list.
	// +kubebuilder:validation:MaxLength=300
	URL string `json:"url"`
}

// McpProjectSpec lists the servers and the agents of one project.
type McpProjectSpec struct {
	// Servers whose tools every agent of the project gets. Empty: no tools, and the agents' tokens are revoked.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=20
	Servers []McpServer `json:"servers,omitempty"`
	// Agents of the project (Agent names). One project per agent (#14).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:items:MaxLength=40
	Agents []string `json:"agents,omitempty"`
}

// McpProjectStatus is written by the operator. It never contains a token.
type McpProjectStatus struct {
	// Ready: True when gateways, virtual server and every agent's token are in place. Reasons for False: InvalidServer,
	// HostNotAllowed, ContextForgeUnavailable, NoTools, SyncFailed.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ContextForge id of the virtual server project-<name>.
	// +optional
	ServerID string `json:"serverID,omitempty"`
	// Names of the tools in the virtual server.
	// +optional
	Tools []string `json:"tools,omitempty"`
	// Token Secrets this McpProject made (agent-<agent>-mcp); a Secret dropped from the spec is revoked and deleted.
	// +optional
	TokenSecrets []string `json:"tokenSecrets,omitempty"`
	// What the operator knows of each token Secret, so it never reads a Secret (#452). No token value.
	// +optional
	// +listType=map
	// +listMapKey=secret
	Tokens []McpToken `json:"tokens,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcpp
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 50",message="McpProject name (the project) must be a DNS label of at most 50 characters"
type McpProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   McpProjectSpec   `json:"spec,omitempty"`
	Status McpProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type McpProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []McpProject `json:"items"`
}

func init() { SchemeBuilder.Register(&McpProject{}, &McpProjectList{}) }

// McpToken is the operator's record of one agent token Secret (#452): the ContextForge token id (not the token), the
// virtual server it is scoped to and when it is re-minted.
type McpToken struct {
	// Secret name, agent-<agent>-mcp.
	Secret string `json:"secret"`
	// ContextForge token id, used to revoke it.
	// +optional
	TokenID string `json:"tokenID,omitempty"`
	// Virtual server the token is scoped to.
	// +optional
	ServerID string `json:"serverID,omitempty"`
	// Unix time after which the token is re-minted.
	// +optional
	RenewAfter int64 `json:"renewAfter,omitempty"`
}
