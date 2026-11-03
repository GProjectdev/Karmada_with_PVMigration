package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestStandaloneMetadataDeepCopy(t *testing.T) {
	v := VolumeMetadata{Standalone: true, PVCSpec: &corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	md := &PVMetadata{Status: PVMetadataStatus{Volumes: []VolumeMetadata{v}, Clusters: []ClusterSnapshot{{Volumes: []VolumeMetadata{v}}}}}
	copy := md.DeepCopy()
	copy.Status.Volumes[0].PVCSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	copy.Status.Clusters[0].Volumes[0].PVCSpec.VolumeName = "changed"
	if md.Status.Volumes[0].PVCSpec.Resources.Requests.Storage().String() != "1Gi" || md.Status.Clusters[0].Volumes[0].PVCSpec.VolumeName != "" {
		t.Fatal("PVC spec aliases the source metadata")
	}
}
