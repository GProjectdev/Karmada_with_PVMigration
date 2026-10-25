package management

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestWorkAdmissionMetadataComparison(t *testing.T) {
	m := testMigration()
	works, err := BuildWorks(m, testMetadata())
	if err != nil {
		t.Fatal(err)
	}
	expected := works[0]
	decorated := expected.DeepCopy()
	manifests, _, _ := unstructured.NestedSlice(decorated.Object, "spec", "workload", "manifests")
	pv := manifests[0].(map[string]interface{})
	metadata := pv["metadata"].(map[string]interface{})
	delete(metadata, "creationTimestamp")
	metadata["labels"].(map[string]interface{})["work.karmada.io/permanent-id"] = "tracking-id"
	metadata["annotations"] = map[string]interface{}{
		workNameAnnotation:                                expected.GetName(),
		workNamespaceAnnotation:                           expected.GetNamespace(),
		"resourcetemplate.karmada.io/managed-labels":      "migration.dcnlab.com/managed-by,migration.dcnlab.com/migration-uid,work.karmada.io/permanent-id",
		"resourcetemplate.karmada.io/managed-annotations": "resourcetemplate.karmada.io/managed-labels,resourcetemplate.karmada.io/managed-annotations",
	}
	_ = unstructured.SetNestedSlice(decorated.Object, manifests, "spec", "workload", "manifests")
	before := decorated.DeepCopy()
	if err := validateWork(decorated, expected, m); err != nil {
		t.Fatalf("admission metadata rejected: %v", err)
	}
	if !reflect.DeepEqual(before.Object, decorated.Object) {
		t.Fatal("comparison mutated Work")
	}
	cases := []struct {
		name  string
		path  []string
		value interface{}
	}{
		{"nfs server", []string{"spec", "nfs", "server"}, "other-server"},
		{"nfs path", []string{"spec", "nfs", "path"}, "/other"},
		{"claim", []string{"spec", "claimRef", "name"}, "other-claim"},
		{"reclaim", []string{"spec", "persistentVolumeReclaimPolicy"}, "Delete"},
		{"owner", []string{"metadata", "labels", OwnerLabel}, "foreign"},
		{"manager", []string{"metadata", "labels", ManagedLabel}, "foreign"},
		{"unknown annotation", []string{"metadata", "annotations", "unexpected"}, "value"},
		{"unknown label", []string{"metadata", "labels", "unexpected"}, "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := decorated.DeepCopy()
			entries, _, _ := unstructured.NestedSlice(actual.Object, "spec", "workload", "manifests")
			_ = unstructured.SetNestedField(entries[0].(map[string]interface{}), tc.value, tc.path...)
			_ = unstructured.SetNestedSlice(actual.Object, entries, "spec", "workload", "manifests")
			if validateWork(actual, expected, m) == nil {
				t.Fatal("unsafe change accepted")
			}
		})
	}
	t.Run("preserve resources", func(t *testing.T) {
		actual := decorated.DeepCopy()
		_ = unstructured.SetNestedField(actual.Object, false, "spec", "preserveResourcesOnDeletion")
		if validateWork(actual, expected, m) == nil {
			t.Fatal("unsafe deletion policy accepted")
		}
	})
}
