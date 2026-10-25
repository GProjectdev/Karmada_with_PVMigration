package management

import (
	"context"
	"reflect"
	"strings"
	"testing"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func selectedTargetOperation() (*api.PVMigration, *unstructured.Unstructured, *unstructured.Unstructured) {
	m := testMigration()
	m.Annotations = map[string]string{"training.dcnlab.com/placement-restore-request-uid": "restore-uid"}
	rb := testBinding(true, []string{targetCluster})
	rb.SetUID("binding-uid")
	rb.SetAnnotations(map[string]string{
		"training.dcnlab.com/placement-restore-request":     "restore",
		"training.dcnlab.com/placement-restore-request-uid": "restore-uid",
		"migration.dcnlab.com/group-volume-hold":            "restore-uid",
		"migration.dcnlab.com/pv-migration":                 m.Name,
		"migration.dcnlab.com/pv-migration-uid":             string(m.UID),
	})
	_ = unstructured.SetNestedField(rb.Object, stsUID, "spec", "resource", "uid")
	_ = unstructured.SetNestedField(rb.Object, testNamespace, "spec", "resource", "namespace")
	request := object(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "RestoreRequest"}, testNamespace, "restore")
	request.SetUID("restore-uid")
	request.Object["spec"] = map[string]interface{}{
		"sourceCluster": sourceCluster, "targetCluster": targetCluster,
		"workloadRef":  map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": stsName, "uid": stsUID},
		"groupRestore": map[string]interface{}{"operationUID": "binding-uid", "sourceWorldUID": stsUID},
	}
	return m, rb, request
}

func TestSelectedTargetUnderExactOperationCanPrepareVolumes(t *testing.T) {
	ctx := context.Background()
	m, rb, request := selectedTargetOperation()
	md := testMetadata()
	c := fakeClient(t, m, md, testStatefulSet(), rb, request)
	mustGet(t, c, client.ObjectKeyFromObject(rb), rb)
	before := rb.DeepCopy()
	r := &PVMigrationReconciler{Client: c}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
			t.Fatal(err)
		}
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Preparing" || len(m.Status.Works) != 1 {
		t.Fatalf("selected suspended target did not prepare: %#v", m.Status)
	}
	ref := m.Status.Works[0]
	work := object(WorkGVK, ref.Namespace, ref.Name)
	mustGet(t, c, client.ObjectKeyFromObject(work), work)
	work.SetGeneration(1) // The fake API does not assign generation on Work creation.
	// Applied alone is insufficient: the member PV phase must be reflected.
	applied := appliedWork(t, m, md, "Available")
	work.Object["status"] = map[string]interface{}{"conditions": applied.Object["status"].(map[string]interface{})["conditions"]}
	mustUpdate(t, c, work)
	if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Preparing" {
		t.Fatalf("accepted absent PV reflection: %#v", m.Status)
	}
	work.Object["status"] = applied.Object["status"]
	mustUpdate(t, c, work)
	if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Ready" {
		t.Fatalf("reflected Available PV not ready: %#v", m.Status)
	}
	mustGet(t, c, client.ObjectKeyFromObject(rb), rb)
	if !reflect.DeepEqual(before.Object, rb.Object) {
		t.Fatal("PV migration must not alter binding placement, suspension or ownership")
	}
}

func TestSelectedTargetLegacyProcessingMarkerDoesNotAuthorizeHold(t *testing.T) {
	ctx := context.Background()
	m, rb, request := selectedTargetOperation()
	m.Spec.TargetCluster = "aws"
	_ = unstructured.SetNestedField(request.Object, "aws", "spec", "targetCluster")
	_ = unstructured.SetNestedSlice(rb.Object, []interface{}{map[string]interface{}{"name": "aws"}}, "spec", "clusters")
	a := rb.GetAnnotations()
	delete(a, "migration.dcnlab.com/group-volume-hold")
	delete(a, "migration.dcnlab.com/pv-migration")
	delete(a, "migration.dcnlab.com/pv-migration-uid")
	a["stwmig.karmada.io/pv-migration"] = "processing"
	rb.SetAnnotations(a)
	md := testMetadata()
	// Even an Applied Work with reflected Available PV cannot replace hold ownership.
	work := appliedWork(t, m, md, "Available")
	c := fakeClient(t, m, md, testStatefulSet(), rb, request, work)
	mustGet(t, c, client.ObjectKeyFromObject(rb), rb)
	mustGet(t, c, client.ObjectKeyFromObject(work), work)
	beforeBinding, beforeWork := rb.DeepCopy(), work.DeepCopy()
	r := &PVMigrationReconciler{Client: c}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
			t.Fatal(err)
		}
		mustGet(t, c, client.ObjectKeyFromObject(m), m)
		if m.Status.Phase != "Pending" || len(m.Status.Works) != 0 ||
			!strings.Contains(m.Status.Message, "same restore hold") {
			t.Fatalf("legacy marker and RR UID link must not authorize selected target: %#v", m.Status)
		}
	}
	mustGet(t, c, client.ObjectKeyFromObject(rb), rb)
	mustGet(t, c, client.ObjectKeyFromObject(work), work)
	if !reflect.DeepEqual(beforeBinding.Object, rb.Object) || !reflect.DeepEqual(beforeWork.Object, work.Object) {
		t.Fatal("refusal must not fabricate receipts or modify binding/Work")
	}
}

func TestSelectedTargetRejectsUnrelatedOrStaleOperations(t *testing.T) {
	tests := []struct {
		name string
		edit func(*api.PVMigration, *unstructured.Unstructured, *unstructured.Unstructured)
	}{
		{"missing migration operation", func(m *api.PVMigration, _, _ *unstructured.Unstructured) { m.Annotations = nil }},
		{"foreign migration operation", func(m *api.PVMigration, _, _ *unstructured.Unstructured) {
			m.Annotations["training.dcnlab.com/placement-restore-request-uid"] = "foreign"
		}},
		{"missing hold", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			delete(a, "migration.dcnlab.com/group-volume-hold")
			rb.SetAnnotations(a)
		}},
		{"foreign hold", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			a["migration.dcnlab.com/group-volume-hold"] = "foreign"
			rb.SetAnnotations(a)
		}},
		{"foreign binding restore", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			a["training.dcnlab.com/placement-restore-request-uid"] = "foreign"
			rb.SetAnnotations(a)
		}},
		{"unbound migration receipt", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			delete(a, "migration.dcnlab.com/pv-migration-uid")
			rb.SetAnnotations(a)
		}},
		{"recreated migration", func(m *api.PVMigration, _, _ *unstructured.Unstructured) { m.UID = "new-migration" }},
		{"foreign migration name", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			a["migration.dcnlab.com/pv-migration"] = "other"
			rb.SetAnnotations(a)
		}},
		{"recreated binding", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) { rb.SetUID("new-binding") }},
		{"missing binding UID", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) { rb.SetUID("") }},
		{"stale binding workload", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(rb.Object, "old-world", "spec", "resource", "uid")
		}},
		{"wrong binding namespace", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(rb.Object, "other", "spec", "resource", "namespace")
		}},
		{"unsuspended", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(rb.Object, false, "spec", "suspension", "dispatching")
		}},
		{"not fenced", func(m *api.PVMigration, _, _ *unstructured.Unstructured) { m.Spec.SourceFenced = false }},
		{"split placement", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(rb.Object, []interface{}{map[string]interface{}{"name": sourceCluster}, map[string]interface{}{"name": targetCluster}}, "spec", "clusters")
		}},
		{"missing request", func(_ *api.PVMigration, rb, _ *unstructured.Unstructured) {
			a := rb.GetAnnotations()
			a["training.dcnlab.com/placement-restore-request"] = "missing"
			rb.SetAnnotations(a)
		}},
		{"recreated request", func(_ *api.PVMigration, _, req *unstructured.Unstructured) { req.SetUID("new-request") }},
		{"wrong operation", func(_ *api.PVMigration, _, req *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(req.Object, "other", "spec", "groupRestore", "operationUID")
		}},
		{"wrong source", func(_ *api.PVMigration, _, req *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(req.Object, "other", "spec", "sourceCluster")
		}},
		{"wrong target", func(_ *api.PVMigration, _, req *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(req.Object, "other", "spec", "targetCluster")
		}},
		{"wrong workload", func(_ *api.PVMigration, _, req *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(req.Object, "old", "spec", "workloadRef", "uid")
		}},
		{"wrong source world", func(_ *api.PVMigration, _, req *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(req.Object, "old", "spec", "groupRestore", "sourceWorldUID")
		}},
		{"invalid claim", func(m *api.PVMigration, _, _ *unstructured.Unstructured) { m.Spec.Volumes[0].TargetPVC = "unrelated" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, rb, request := selectedTargetOperation()
			tt.edit(m, rb, request)
			c := fakeClient(t, m, testMetadata(), testStatefulSet(), rb, request)
			r := &PVMigrationReconciler{Client: c}
			if _, err := r.Reconcile(context.Background(), reconcileRequest(m)); err != nil {
				t.Fatal(err)
			}
			mustGet(t, c, client.ObjectKeyFromObject(m), m)
			if m.Status.Phase != "Pending" || len(m.Status.Works) != 0 {
				t.Fatalf("unsafe operation allowed: %#v", m.Status)
			}
		})
	}
}
