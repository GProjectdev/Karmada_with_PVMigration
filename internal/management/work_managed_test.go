package management

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestNewPVWorkManagedIdentity(t *testing.T) {
	m, md := testMigration(), testMetadata()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		entries, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
		pv := entries[0].(map[string]interface{})
		label, _, _ := unstructured.NestedString(pv, "metadata", "labels", "karmada.io/managed")
		if label != "true" {
			t.Fatal("PV must be managed by Karmada on creation")
		}
		name, _, _ := unstructured.NestedString(pv, "metadata", "annotations", workNameAnnotation)
		ns, _, _ := unstructured.NestedString(pv, "metadata", "annotations", workNamespaceAnnotation)
		if name != w.GetName() || ns != w.GetNamespace() {
			t.Fatal("incorrect routing")
		}
		oldHash, _ := planHash(m, md, works)
		unstructured.RemoveNestedField(pv, "metadata", "labels", "karmada.io/managed")
		old := w.DeepCopy()
		_ = unstructured.SetNestedSlice(old.Object, entries, "spec", "workload", "manifests")
		if validateWork(old, w, m) == nil {
			t.Fatal("unmanaged legacy Work must not be silently adopted")
		}
		legacyHash, _ := planHash(m, md, []*unstructured.Unstructured{old})
		if oldHash == legacyHash {
			t.Fatal("management identity must participate in plan hash")
		}
	}
}
