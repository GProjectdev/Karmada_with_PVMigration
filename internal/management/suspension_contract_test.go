package management

import (
	"context"
	"reflect"
	"testing"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTwoVolumeCleanupNeverResumesResourceBinding(t *testing.T) {
	ctx := context.Background()
	m, md := testMigration(), testMetadata()
	first := appliedWork(t, m, md, "Bound")
	secondMigration := m.DeepCopy()
	secondMigration.Spec.Volumes[0] = api.VolumeMapping{SourcePVC: "data-web-1", TargetPVC: "data-web-2"}
	secondMD := md.DeepCopy()
	v := &secondMD.Status.Clusters[0].Volumes[0]
	v.PVCName, v.PVCUID, v.PVName, v.Ordinal = "data-web-1", "pvc-uid-2", "pv-data-web-1", 1
	v.PVSpec.ClaimRef.Name, v.PVSpec.ClaimRef.UID = "data-web-1", "pvc-uid-2"
	second := appliedWork(t, secondMigration, secondMD, "Bound")
	m.Spec.Volumes = append(m.Spec.Volumes, secondMigration.Spec.Volumes[0])
	m.Status = api.PVMigrationStatus{
		Phase: "Ready", ObservedGeneration: m.Generation, PlanHash: "immutable-plan",
		Works: []api.WorkReference{
			{Name: first.GetName(), Namespace: first.GetNamespace(), Applied: true},
			{Name: second.GetName(), Namespace: second.GetNamespace(), Applied: true},
		},
	}
	rb := testBinding(true, []string{sourceCluster})
	originalSpec := rb.DeepCopy().Object["spec"]
	c := fakeClient(t, m, first, second, rb)
	r := &PVCleanupReconciler{Client: c}
	req := reconcileRequest(m)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Ready" {
		t.Fatalf("must observe both Work deletions before completion: %#v", m.Status)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Completed" || len(m.Status.Works) != 2 {
		t.Fatalf("missing complete two-volume evidence: %#v", m.Status)
	}
	for _, ref := range m.Status.Works {
		if !ref.Applied || !ref.Detached {
			t.Fatalf("incomplete Work: %#v", ref)
		}
	}
	mustGet(t, c, client.ObjectKeyFromObject(rb), rb)
	suspended, _, _ := unstructured.NestedBool(rb.Object, "spec", "suspension", "dispatching")
	if !suspended || !reflect.DeepEqual(originalSpec, rb.Object["spec"]) {
		t.Fatal("PV completion must not change workload dispatch or placement")
	}
}
