package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Collection timestamps change on every poll, but a started plan's identities/specs must not.
func planHash(m *api.PVMigration, md *api.PVMetadata, works []*unstructured.Unstructured) (string, error) {
	parts := []interface{}{}
	for _, snapshot := range md.Status.Clusters {
		if snapshot.ClusterName != m.Spec.SourceCluster {
			continue
		}
		parts = append(parts, snapshot.WorkloadUID)
		for _, mapping := range m.Spec.Volumes {
			for _, volume := range snapshot.Volumes {
				if volume.PVCName == mapping.SourcePVC {
					parts = append(parts, []string{volume.PVCName, volume.PVCUID, volume.PVName})
				}
			}
		}
	}
	for _, w := range works {
		// Routing is derived from Work identity, not part of the storage plan.
		// Preserve hashes already persisted by versions without routing metadata.
		legacy := w.DeepCopy()
		removeWorkRouting(legacy)
		parts = append(parts, legacy.Object["spec"])
	}
	data, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
