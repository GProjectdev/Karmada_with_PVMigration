package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRoutingPreservesLegacyPlanHash(t *testing.T) {
	m, md := testMigration(), testMetadata()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	parts := []interface{}{md.Status.Clusters[0].WorkloadUID}
	v := md.Status.Clusters[0].Volumes[0]
	parts = append(parts, []string{v.PVCName, v.PVCUID, v.PVName})
	legacy := works[0].DeepCopy()
	removeWorkRouting(legacy)
	parts = append(parts, legacy.Object["spec"])
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	got, err := planHash(m, md, works)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy hash changed: %s %v", got, err)
	}
}

func TestLegacyRoutingRepairAndCleanup(t *testing.T) {
	ctx := context.Background()
	m, md := testMigration(), testMetadata()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	expected := works[0]
	legacy := expected.DeepCopy()
	removeWorkRouting(legacy)
	legacy.SetUID("old-work-uid")
	legacy.SetGeneration(1)
	hash, _ := planHash(m, md, works)
	m.Status = api.PVMigrationStatus{Phase: "Preparing", PlanHash: hash, ObservedGeneration: m.Generation,
		Works: []api.WorkReference{{Name: legacy.GetName(), Namespace: legacy.GetNamespace()}}}
	c := fakeClient(t, m, md, legacy)
	r := &PVMigrationReconciler{Client: c}
	if changed, err := r.ensureWorkRouting(ctx, m, 0, legacy, expected); !changed || err != nil {
		t.Fatalf("intent: %v", err)
	}
	actual := object(WorkGVK, legacy.GetNamespace(), legacy.GetName())
	mustGet(t, c, client.ObjectKeyFromObject(actual), actual)
	if !reflect.DeepEqual(actual.Object["spec"], legacy.Object["spec"]) {
		t.Fatal("spec changed before intent persisted")
	}
	// Recreate reconciler to exercise restart after intent.
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	r = &PVMigrationReconciler{Client: c}
	if changed, err := r.ensureWorkRouting(ctx, m, 0, actual, expected); !changed || err != nil {
		t.Fatalf("repair: %v", err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(actual), actual)
	// The fake client does not implement API server generation increments.
	actual.SetGeneration(2)
	mustUpdate(t, c, actual)
	if err := validateWork(actual, expected, m); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"uid", "generation", "path", "route"} {
		bad := actual.DeepCopy()
		switch change {
		case "uid":
			bad.SetUID("replacement")
		case "generation":
			bad.SetGeneration(3)
		default:
			entries, _, _ := unstructured.NestedSlice(bad.Object, "spec", "workload", "manifests")
			obj := entries[0].(map[string]interface{})
			if change == "path" {
				_ = unstructured.SetNestedField(obj, "/wrong", "spec", "nfs", "path")
			} else {
				_ = unstructured.SetNestedField(obj, "other", "metadata", "annotations", workNameAnnotation)
			}
			_ = unstructured.SetNestedSlice(bad.Object, entries, "spec", "workload", "manifests")
		}
		if validateWork(bad, expected, m) == nil {
			t.Fatalf("repaired Work accepted %s change", change)
		}
	}
	if changed, err := r.ensureWorkRouting(ctx, m, 0, actual, expected); changed || err != nil {
		t.Fatalf("repair not idempotent: %v", err)
	}
	if workApplied(actual) {
		t.Fatal("repair fabricated reflection")
	}
	fresh := appliedWork(t, m, md, "Available")
	actual.Object["status"] = fresh.Object["status"]
	conditions, _, _ := unstructured.NestedSlice(actual.Object, "status", "conditions")
	conditions[0].(map[string]interface{})["observedGeneration"] = int64(2)
	_ = unstructured.SetNestedSlice(actual.Object, conditions, "status", "conditions")
	mustUpdate(t, c, actual)
	m.Status.Phase = "Ready"
	m.Status.Works[0].Applied = true
	mustStatusUpdate(t, c, m)
	cleanup := &PVCleanupReconciler{Client: c}
	if _, err := cleanup.Reconcile(ctx, reconcileRequest(m)); err != nil {
		t.Fatal(err)
	}
	if exists(ctx, c, WorkGVK, actual.GetNamespace(), actual.GetName()) {
		t.Fatal("verified repaired Work not detached")
	}
	if _, err := cleanup.Reconcile(ctx, reconcileRequest(m)); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Completed" {
		t.Fatal(m.Status.Phase)
	}
}

func TestRoutingRepairRejectsForeignAndChangedWork(t *testing.T) {
	for _, kind := range []string{"foreign-route", "foreign-owner", "nfs", "generation", "replacement", "stale-status"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			m, md := testMigration(), testMetadata()
			works, _ := BuildWorks(m, md)
			w := works[0].DeepCopy()
			removeWorkRouting(w)
			w.SetUID("uid")
			w.SetGeneration(1)
			m.Status.Works = []api.WorkReference{{Name: w.GetName(), Namespace: w.GetNamespace()}}
			switch kind {
			case "generation":
				w.SetGeneration(2)
			case "replacement":
				m.Status.Works[0].RoutingWorkUID = "other"
				m.Status.Works[0].RoutingGeneration = 2
			case "foreign-owner":
				w.SetLabels(map[string]string{ManagedLabel: ManagerName, OwnerLabel: "foreign"})
			case "stale-status":
				w.Object["status"] = appliedWork(t, m, md, "Available").Object["status"]
			default:
				entries, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
				obj := entries[0].(map[string]interface{})
				if kind == "nfs" {
					_ = unstructured.SetNestedField(obj, "/wrong", "spec", "nfs", "path")
				} else {
					_ = unstructured.SetNestedField(obj, "other", "metadata", "annotations", workNameAnnotation)
				}
				_ = unstructured.SetNestedSlice(w.Object, entries, "spec", "workload", "manifests")
			}
			c := fakeClient(t, m, w)
			r := &PVMigrationReconciler{Client: c}
			if _, err := r.ensureWorkRouting(ctx, m, 0, w, works[0]); err == nil {
				t.Fatal("unsafe repair accepted")
			}
		})
	}
}
