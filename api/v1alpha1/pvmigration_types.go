package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type VolumeMapping struct {
	SourcePVC string `json:"sourcePVC,omitempty"`
	TargetPVC string `json:"targetPVC,omitempty"`
}

type WorkReference struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Applied   bool   `json:"applied,omitempty"`
	Detached  bool   `json:"detached,omitempty"`
}

type PVMigrationSpec struct {
	MetadataRef     string          `json:"metadataRef,omitempty"`
	SourceCluster   string          `json:"sourceCluster,omitempty"`
	TargetCluster   string          `json:"targetCluster,omitempty"`
	ResourceBinding string          `json:"resourceBinding,omitempty"`
	SourceFenced    bool            `json:"sourceFenced,omitempty"`
	Volumes         []VolumeMapping `json:"volumes,omitempty"`
}

type PVMigrationStatus struct {
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Phase              string          `json:"phase,omitempty"`
	Message            string          `json:"message,omitempty"`
	PlanHash           string          `json:"planHash,omitempty"`
	Works              []WorkReference `json:"works,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type PVMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PVMigrationSpec   `json:"spec,omitempty"`
	Status PVMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PVMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PVMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PVMigration{}, &PVMigrationList{})
}
