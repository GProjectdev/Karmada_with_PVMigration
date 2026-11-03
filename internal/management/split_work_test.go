package management

import (
	"context"
	"testing"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func boundSplitWorks(t *testing.T, m *api.PVMigration, md *api.PVMetadata) []*unstructured.Unstructured {
	t.Helper()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	pair := boundPair(t, m, md)
	statuses, _, _ := unstructured.NestedSlice(pair.Object, "status", "manifestStatuses")
	for i, w := range works {
		w.SetGeneration(1)
		w.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Applied", "status": "True", "observedGeneration": int64(1)}}, "manifestStatuses": []interface{}{statuses[i]}}
	}
	return works
}

func TestSplitWorkPlanAndBinding(t *testing.T) {
	for _, mutation := range []string{"valid", "missing", "pending", "wrong-uid", "wrong-nfs", "generation", "owner", "duplicate-status", "deleted-receipt"} {
		t.Run(mutation, func(t *testing.T) {
			m, md, _ := standaloneFixture()
			works := boundSplitWorks(t, m, md)
			if len(works) != 2 || works[1].GetName() != works[0].GetName()+"-pvc" {
				t.Fatal("not split")
			}
			for _, w := range works {
				manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
				if len(manifests) != 1 {
					t.Fatal("upstream supports single reflection per Work")
				}
				routing, _, _ := unstructured.NestedString(manifests[0].(map[string]interface{}), "metadata", "annotations", workNameAnnotation)
				if routing != w.GetName() {
					t.Fatal("wrong Work routing")
				}
			}
			statuses, _, _ := unstructured.NestedSlice(works[1].Object, "status", "manifestStatuses")
			s := statuses[0].(map[string]interface{})
			switch mutation {
			case "missing":
				works[0] = nil
			case "pending":
				_ = unstructured.SetNestedField(s, "Pending", "status", "phase")
			case "wrong-uid":
				_ = unstructured.SetNestedField(s, "other", "status", "uid")
			case "wrong-nfs":
				pvs, _, _ := unstructured.NestedSlice(works[0].Object, "status", "manifestStatuses")
				_ = unstructured.SetNestedField(pvs[0].(map[string]interface{}), "/wrong", "status", "nfs", "path")
				_ = unstructured.SetNestedSlice(works[0].Object, pvs, "status", "manifestStatuses")
			case "generation":
				works[1].SetGeneration(2)
			case "owner":
				works[1].SetLabels(map[string]string{standalonePairLabel: works[0].GetName(), OwnerLabel: "other"})
			case "duplicate-status":
				statuses = append(statuses, s)
			case "deleted-receipt":
				_ = unstructured.SetNestedField(s, "now", "status", "deletionTimestamp")
			}
			_ = unstructured.SetNestedSlice(works[1].Object, statuses, "status", "manifestStatuses")
			pvUID, pvcUID, ready := splitPairApplied(works[0], works[1])
			if ready != (mutation == "valid") {
				t.Fatalf("ready=%v", ready)
			}
			if ready && (pvUID != "member-pv" || pvcUID != "member-pvc") {
				t.Fatal("missing binding receipt")
			}
		})
	}
}

func TestSplitCleanupRejectsChangedBinding(t *testing.T) {
	m, md, _ := standaloneFixture()
	works := boundSplitWorks(t, m, md)
	hash, err := planHash(m, md, works)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = api.PVMigrationStatus{Phase: "Ready", ObservedGeneration: m.Generation, PlanHash: hash}
	for _, w := range works {
		m.Status.Works = append(m.Status.Works, api.WorkReference{Name: w.GetName(), Namespace: w.GetNamespace(), Standalone: true, Applied: true, PVUID: "member-pv", PVCUID: "member-pvc"})
	}
	s, _, _ := unstructured.NestedSlice(works[0].Object, "status", "manifestStatuses")
	_ = unstructured.SetNestedField(s[0].(map[string]interface{}), "recreated", "status", "uid")
	_ = unstructured.SetNestedSlice(works[0].Object, s, "status", "manifestStatuses")
	c := fakeClient(t, m, md, works[0], works[1])
	r := &PVCleanupReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err == nil {
		t.Fatal("changed UID detached")
	}
}

func TestSplitCleanupResumesAfterFirstWorkDeleted(t *testing.T) {
	m, md, _ := standaloneFixture()
	works := boundSplitWorks(t, m, md)
	hash, err := planHash(m, md, works)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = api.PVMigrationStatus{Phase: "Ready", ObservedGeneration: m.Generation, PlanHash: hash}
	for _, w := range works {
		m.Status.Works = append(m.Status.Works, api.WorkReference{Name: w.GetName(), Namespace: w.GetNamespace(), Standalone: true, Applied: true, PVUID: "member-pv", PVCUID: "member-pvc"})
	}
	// Simulate a crash after the PV Work deletion, before status was persisted.
	c := fakeClient(t, m, md, works[1])
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	for i := 0; i < 2; i++ {
		r := &PVCleanupReconciler{Client: c}
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	got := &api.PVMigration{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Completed" {
		t.Fatalf("phase=%s", got.Status.Phase)
	}
	for _, ref := range got.Status.Works {
		if !ref.Detached || ref.PVUID != "member-pv" || ref.PVCUID != "member-pvc" {
			t.Fatal("lost durable binding receipt")
		}
	}
}
