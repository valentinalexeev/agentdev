package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Project (#54) groups the repositories of one owner that a team works on, and carries the project's own section of the
// harness guide (#206). Part 1 (this type): repositories, guideAppend, and Agent.spec.project. The multi-repo agent loop
// and the ProjectSecret scope for a whole project follow in later parts (design on #54).

// ProjectSpec is the desired project.
// +kubebuilder:validation:XValidation:rule="self.repos.all(r, r.split('/')[0] == self.repos[0].split('/')[0])",message="all repos of a project must have the same owner (one App installation)"
type ProjectSpec struct {
	// Forge kind of all repositories.
	// +kubebuilder:validation:Enum=gitea;github
	// +kubebuilder:default=gitea
	Forge string `json:"forge,omitempty"`
	// Repositories of the project, owner/name, all of the same owner.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=set
	// Each part starts with a letter, digit or _, has no "..", and does not end with a dot (as the API's validRepo).
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_]([A-Za-z0-9_-]|\.[A-Za-z0-9_-])*/[A-Za-z0-9_]([A-Za-z0-9_-]|\.[A-Za-z0-9_-])*$`
	// +kubebuilder:validation:items:MaxLength=201
	Repos []string `json:"repos"`
	// Text that project members add to the harness guide of every agent of the project. Appended under its own
	// heading after the fixed agentdev part; it never replaces that part. Plain text, at most 8 KiB.
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	GuideAppend string `json:"guideAppend,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=proj
// +kubebuilder:printcolumn:name="Forge",type=string,JSONPath=`.spec.forge`
// +kubebuilder:printcolumn:name="Repos",type=string,JSONPath=`.spec.repos`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Project is a group of repositories of one owner with its own harness guide section.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList is a list of Projects.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() { SchemeBuilder.Register(&Project{}, &ProjectList{}) }
