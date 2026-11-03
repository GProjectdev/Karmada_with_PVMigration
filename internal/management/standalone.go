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

const standalonePairLabel = "migration.dcnlab.com/standalone-pair"

func standaloneWork(w *unstructured.Unstructured) bool {
	return w.GetLabels()[standalonePairLabel] != ""
}

// Check the single reflected resource before combining independent Work receipts.
func standaloneStatus(w *unstructured.Unstructured) (map[string]interface{}, bool) {
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	statuses, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
	if len(manifests) != 1 || len(statuses) != 1 {
		return nil, false
	}
	manifest, ok := manifests[0].(map[string]interface{})
	reflection, valid := statuses[0].(map[string]interface{})
	if !ok || !valid {
		return nil, false
	}
	obj := &unstructured.Unstructured{Object: manifest}
	pair := w.GetLabels()[standalonePairLabel]
	if pair == "" || obj.GetAPIVersion() != "v1" || obj.GetName() == "" {
		return nil, false
	}
	for key, want := range map[string]string{"group": "", "version": "v1", "kind": obj.GetKind(), "name": obj.GetName(), "namespace": obj.GetNamespace()} {
		got, _, err := unstructured.NestedString(reflection, "identifier", key)
		if err != nil || got != want {
			return nil, false
		}
	}
	status, _, _ := unstructured.NestedMap(reflection, "status")
	uid, _, err := unstructured.NestedString(status, "uid")
	if err != nil || uid == "" || status["phase"] != "Bound" || status["deletionTimestamp"] != nil || w.GetLabels()[OwnerLabel] == "" || status["migrationUID"] != w.GetLabels()[OwnerLabel] {
		return nil, false
	}
	switch obj.GetKind() {
	case "PersistentVolume":
		if w.GetName() != pair || obj.GetName() != pair || obj.GetNamespace() != "" {
			return nil, false
		}
		for _, key := range []string{"name", "namespace"} {
			want, _, _ := unstructured.NestedString(obj.Object, "spec", "claimRef", key)
			got, _, _ := unstructured.NestedString(status, "claimRef", key)
			if want == "" || want != got {
				return nil, false
			}
		}
		claimUID, _, _ := unstructured.NestedString(status, "claimRef", "uid")
		var expected, actual corev1.NFSVolumeSource
		nfs, _, _ := unstructured.NestedMap(obj.Object, "spec", "nfs")
		backing, _, _ := unstructured.NestedMap(status, "nfs")
		if claimUID == "" || runtime.DefaultUnstructuredConverter.FromUnstructured(nfs, &expected) != nil || runtime.DefaultUnstructuredConverter.FromUnstructured(backing, &actual) != nil || expected.Server == "" || expected.Path == "" || !reflect.DeepEqual(expected, actual) {
			return nil, false
		}
	case "PersistentVolumeClaim":
		volume, _, _ := unstructured.NestedString(obj.Object, "spec", "volumeName")
		if w.GetName() != pair+"-pvc" || obj.GetNamespace() == "" || volume != pair || status["volumeName"] != pair {
			return nil, false
		}
	default:
		return nil, false
	}
	return status, true
}

func splitPairApplied(pv, pvc *unstructured.Unstructured) (string, string, bool) {
	if pv == nil || pvc == nil || pv.GetNamespace() != pvc.GetNamespace() || pv.GetLabels()[OwnerLabel] != pvc.GetLabels()[OwnerLabel] || pv.GetLabels()[standalonePairLabel] != pvc.GetLabels()[standalonePairLabel] || !standaloneWork(pv) || !standaloneWork(pvc) || !workApplied(pv) || !workApplied(pvc) {
		return "", "", false
	}
	manifests, statuses := []interface{}{}, []interface{}{}
	for _, w := range []*unstructured.Unstructured{pv, pvc} {
		objects, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
		reflections, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
		manifests = append(manifests, objects...)
		statuses = append(statuses, reflections...)
	}
	if !pairedWorkApplied(pv, manifests, statuses) {
		return "", "", false
	}
	pvs, _ := standaloneStatus(pv)
	pvcs, _ := standaloneStatus(pvc)
	return pvs["uid"].(string), pvcs["uid"].(string), true
}

func standaloneReceiptMatches(w *unstructured.Unstructured, ref api.WorkReference) bool {
	status, valid := standaloneStatus(w)
	if !valid || ref.PVUID == "" || ref.PVCUID == "" {
		return false
	}
	if w.GetName() == w.GetLabels()[standalonePairLabel] {
		claimUID, _, _ := unstructured.NestedString(status, "claimRef", "uid")
		return status["uid"] == ref.PVUID && claimUID == ref.PVCUID
	}
	return status["uid"] == ref.PVCUID
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
