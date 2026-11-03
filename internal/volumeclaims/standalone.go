package volumeclaims

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
)

// Standalone returns explicit and admission-injected claim names. StatefulSet
// claim templates take precedence over same-named pod-template volumes.
func Standalone(sts *appsv1.StatefulSet) []string {
	templates := map[string]bool{}
	for _, template := range sts.Spec.VolumeClaimTemplates {
		templates[template.Name] = true
	}
	claims := map[string]bool{}
	for _, volume := range sts.Spec.Template.Spec.Volumes {
		if !templates[volume.Name] && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			claims[volume.PersistentVolumeClaim.ClaimName] = true
		}
	}
	if name := sts.Spec.Template.Annotations["fluidcr.dcnlab.com/checkpoint-claim"]; name != "" {
		claims[name] = true
	}
	// Explicit aliases of generated claims still use the VCT migration lane.
	replicas, start := int32(1), int32(0)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	if sts.Spec.Ordinals != nil {
		start = sts.Spec.Ordinals.Start
	}
	for _, template := range sts.Spec.VolumeClaimTemplates {
		for offset := int32(0); offset < replicas; offset++ {
			delete(claims, fmt.Sprintf("%s-%s-%d", template.Name, sts.Name, int64(start)+int64(offset)))
		}
	}
	result := make([]string, 0, len(claims))
	for name := range claims {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
