package management

import (
	"context"
	"fmt"
	"reflect"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const workNameAnnotation = "work.karmada.io/name"
const workNamespaceAnnotation = "work.karmada.io/namespace"

func setWorkRouting(w *unstructured.Unstructured) {
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	for _, entry := range manifests {
		obj := entry.(map[string]interface{})
		_ = unstructured.SetNestedField(obj, w.GetName(), "metadata", "annotations", workNameAnnotation)
		_ = unstructured.SetNestedField(obj, w.GetNamespace(), "metadata", "annotations", workNamespaceAnnotation)
	}
	_ = unstructured.SetNestedSlice(w.Object, manifests, "spec", "workload", "manifests")
}

func removeWorkRouting(w *unstructured.Unstructured) {
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	for _, entry := range manifests {
		obj := entry.(map[string]interface{})
		unstructured.RemoveNestedField(obj, "metadata", "annotations", workNameAnnotation)
		unstructured.RemoveNestedField(obj, "metadata", "annotations", workNamespaceAnnotation)
		annotations, found, _ := unstructured.NestedMap(obj, "metadata", "annotations")
		if found && len(annotations) == 0 {
			unstructured.RemoveNestedField(obj, "metadata", "annotations")
		}
	}
	_ = unstructured.SetNestedSlice(w.Object, manifests, "spec", "workload", "manifests")
}

func workGenerationAllowed(w *unstructured.Unstructured, m *api.PVMigration) bool {
	for _, ref := range m.Status.Works {
		if ref.Name == w.GetName() && ref.Namespace == w.GetNamespace() && ref.RoutingWorkUID != "" {
			return ref.RoutingGeneration == 2 && ref.RoutingWorkUID == string(w.GetUID()) &&
				w.GetGeneration() == ref.RoutingGeneration
		}
	}
	return w.GetGeneration() <= 1
}

// Persist a UID-bound intent before the sole permitted generation-1 -> 2 edit.
// Only a legacy Work with no reflected status may be repaired automatically.
func (r *PVMigrationReconciler) ensureWorkRouting(ctx context.Context, m *api.PVMigration, index int, actual, expected *unstructured.Unstructured) (bool, error) {
	ref := &m.Status.Works[index]
	if actual.GetDeletionTimestamp() != nil {
		return false, fmt.Errorf("Work is terminating")
	}
	if reflect.DeepEqual(comparableWorkSpec(actual), comparableWorkSpec(expected)) {
		return false, nil
	}
	legacy := expected.DeepCopy()
	removeWorkRouting(legacy)
	if actual.GetGeneration() != 1 || actual.GetUID() == "" || ref.Applied || ref.Detached {
		return false, fmt.Errorf("Work %s ownership or immutable spec conflict: refuse routing repair of modified/applied Work", actual.GetName())
	}
	// validateWork's normal generation gate intentionally rejects a pending intent.
	check := m.DeepCopy()
	check.Status.Works[index].RoutingWorkUID = ""
	if err := validateWork(actual, legacy, check); err != nil {
		return false, err
	}
	statuses, _, _ := unstructured.NestedSlice(actual.Object, "status", "manifestStatuses")
	if len(statuses) != 0 {
		return false, fmt.Errorf("refuse routing repair with preexisting reflection")
	}
	if ref.RoutingWorkUID == "" {
		base := m.DeepCopy()
		ref.RoutingWorkUID = string(actual.GetUID())
		ref.RoutingGeneration = 2
		return true, r.Status().Patch(ctx, m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	if ref.RoutingWorkUID != string(actual.GetUID()) || ref.RoutingGeneration != 2 {
		return false, fmt.Errorf("routing repair identity changed")
	}
	base := actual.DeepCopy()
	setWorkRouting(actual)
	return true, r.Patch(ctx, actual, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
