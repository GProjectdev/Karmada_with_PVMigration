package management

import (
	"fmt"
	"reflect"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func pairedWork(w *unstructured.Unstructured) bool {
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	return len(manifests) == 2
}

func standaloneClaim(m *api.PVMigration, v api.VolumeMetadata, pvName string) (map[string]interface{}, error) {
	spec := v.PVCSpec.DeepCopy()
	if spec.VolumeName != v.PVName || v.PVName == "" || spec.Selector != nil || spec.DataSource != nil || spec.DataSourceRef != nil || spec.VolumeAttributesClassName != nil {
		return nil, fmt.Errorf("standalone PVC %s has mismatched source PV or unsupported selector/data source/attributes", v.PVCName)
	}
	storage := spec.Resources.Requests[corev1.ResourceStorage]
	capacity := v.PVSpec.Capacity[corev1.ResourceStorage]
	if storage.Sign() <= 0 || storage.Cmp(capacity) > 0 || len(spec.AccessModes) == 0 {
		return nil, fmt.Errorf("standalone PVC %s has invalid capacity or access modes", v.PVCName)
	}
	for _, mode := range spec.AccessModes {
		found := false
		for _, pvMode := range v.PVSpec.AccessModes {
			found = found || mode == pvMode
		}
		if !found {
			return nil, fmt.Errorf("standalone PVC access mode is incompatible with source PV")
		}
	}
	volumeMode := func(mode *corev1.PersistentVolumeMode) corev1.PersistentVolumeMode {
		if mode == nil {
			return corev1.PersistentVolumeFilesystem
		}
		return *mode
	}
	if volumeMode(spec.VolumeMode) != volumeMode(v.PVSpec.VolumeMode) || volumeMode(spec.VolumeMode) != corev1.PersistentVolumeFilesystem {
		return nil, fmt.Errorf("standalone NFS PVC requires matching filesystem volume mode")
	}
	class := ""
	if spec.StorageClassName != nil {
		class = *spec.StorageClassName
	}
	if class != v.PVSpec.StorageClassName {
		return nil, fmt.Errorf("standalone PVC storage class differs from source PV")
	}
	// Pin the empty class too: never let a target default class provision storage.
	spec.StorageClassName = &class
	spec.VolumeName = pvName
	claim := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: v.PVCName, Namespace: m.Namespace, Labels: map[string]string{ManagedLabel: ManagerName, OwnerLabel: string(m.UID), "karmada.io/managed": "true"}},
		Spec:       *spec,
	}
	manifest, err := runtime.DefaultUnstructuredConverter.ToUnstructured(claim)
	delete(manifest, "status")
	return manifest, err
}

// A standalone pair is ready only after reciprocal, UID-bound binding on the
// member. Applied alone also covers pending claims and is not sufficient.
func pairedWorkApplied(w *unstructured.Unstructured, manifests, statuses []interface{}) bool {
	if len(statuses) != 2 {
		return false
	}
	objects := map[string]*unstructured.Unstructured{}
	reflections := map[string]map[string]interface{}{}
	for _, entry := range manifests {
		obj, ok := entry.(map[string]interface{})
		if !ok {
			return false
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.GetAPIVersion() != "v1" || objects[u.GetKind()] != nil || u.GetName() == "" {
			return false
		}
		objects[u.GetKind()] = u
	}
	for _, entry := range statuses {
		s, ok := entry.(map[string]interface{})
		if !ok {
			return false
		}
		kind, _, _ := unstructured.NestedString(s, "identifier", "kind")
		obj := objects[kind]
		if obj == nil || reflections[kind] != nil {
			return false
		}
		for key, want := range map[string]string{"group": "", "version": "v1", "kind": obj.GetKind(), "name": obj.GetName(), "namespace": obj.GetNamespace()} {
			got, _, err := unstructured.NestedString(s, "identifier", key)
			if err != nil || got != want {
				return false
			}
		}
		status, _, _ := unstructured.NestedMap(s, "status")
		uid, _, err := unstructured.NestedString(status, "uid")
		if err != nil || uid == "" || status["deletionTimestamp"] != nil || status["phase"] != "Bound" || status["migrationUID"] != w.GetLabels()[OwnerLabel] || w.GetLabels()[OwnerLabel] == "" {
			return false
		}
		reflections[kind] = status
	}
	pv, pvc := objects["PersistentVolume"], objects["PersistentVolumeClaim"]
	if pv == nil || pvc == nil {
		return false
	}
	pvs, pvcs := reflections["PersistentVolume"], reflections["PersistentVolumeClaim"]
	if pvcs["volumeName"] != pv.GetName() {
		return false
	}
	for key, want := range map[string]interface{}{"name": pvc.GetName(), "namespace": pvc.GetNamespace(), "uid": pvcs["uid"]} {
		got, _, _ := unstructured.NestedFieldNoCopy(pvs, "claimRef", key)
		if got != want {
			return false
		}
	}
	var expected, actual corev1.NFSVolumeSource
	nfs, _, _ := unstructured.NestedMap(pv.Object, "spec", "nfs")
	reflectedNFS, _, _ := unstructured.NestedMap(pvs, "nfs")
	if runtime.DefaultUnstructuredConverter.FromUnstructured(nfs, &expected) != nil || runtime.DefaultUnstructuredConverter.FromUnstructured(reflectedNFS, &actual) != nil {
		return false
	}
	return expected.Server != "" && expected.Path != "" && reflect.DeepEqual(expected, actual)
}
