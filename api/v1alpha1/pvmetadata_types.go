package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadRef struct {
	Name string `json:"name,omitempty"`
	UID  string `json:"uid,omitempty"`
}

type VolumeMetadata struct {
	PVCName      string                      `json:"pvcName,omitempty"`
	PVCUID       string                      `json:"pvcUID,omitempty"`
	TemplateName string                      `json:"templateName,omitempty"`
	Ordinal      int32                       `json:"ordinal,omitempty"`
	PVName       string                      `json:"pvName,omitempty"`
	PVSpec       corev1.PersistentVolumeSpec `json:"pvSpec,omitempty"`
}

type ClusterSnapshot struct {
	ClusterName        string           `json:"clusterName,omitempty"`
	ObservedGeneration int64            `json:"observedGeneration,omitempty"`
	Ready              bool             `json:"ready,omitempty"`
	Message            string           `json:"message,omitempty"`
	CollectedAt        *metav1.Time     `json:"collectedAt,omitempty"`
	WorkloadUID        string           `json:"workloadUID,omitempty"`
	Volumes            []VolumeMetadata `json:"volumes,omitempty"`
}

type PVMetadataSpec struct {
	WorkloadRef   WorkloadRef `json:"workloadRef,omitempty"`
	SourceCluster string      `json:"sourceCluster,omitempty"`
}

type PVMetadataStatus struct {
	ObservedGeneration int64             `json:"observedGeneration,omitempty"`
	Ready              bool              `json:"ready,omitempty"`
	Message            string            `json:"message,omitempty"`
	CollectedAt        *metav1.Time      `json:"collectedAt,omitempty"`
	WorkloadUID        string            `json:"workloadUID,omitempty"`
	Volumes            []VolumeMetadata  `json:"volumes,omitempty"`
	Clusters           []ClusterSnapshot `json:"clusters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type PVMetadata struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PVMetadataSpec   `json:"spec,omitempty"`
	Status PVMetadataStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PVMetadataList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PVMetadata `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PVMetadata{}, &PVMetadataList{})
}
