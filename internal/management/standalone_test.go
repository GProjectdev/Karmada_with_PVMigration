package management

import (
	"context"
	"reflect"
	"testing"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func standaloneFixture() (*api.PVMigration, *api.PVMetadata, *appsv1.StatefulSet) {
	m, md, sts := testMigration(), testMetadata(), testStatefulSet()
	m.Spec.Volumes = []api.VolumeMapping{{SourcePVC: "checkpoint", TargetPVC: "checkpoint"}}
	v := &md.Status.Clusters[0].Volumes[0]
	v.PVCName = "checkpoint"
	v.TemplateName = ""
	v.Standalone = true
	v.PVSpec.ClaimRef.Name = "checkpoint"
	v.PVCSpec = &corev1.PersistentVolumeClaimSpec{VolumeName: v.PVName, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}
	sts.Spec.VolumeClaimTemplates = nil
	sts.Spec.Template.Annotations = map[string]string{"fluidcr.dcnlab.com/checkpoint-claim": "checkpoint"}
	return m, md, sts
}

func boundPair(t *testing.T, m *api.PVMigration, md *api.PVMetadata) *unstructured.Unstructured {
	t.Helper()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	w := works[0]
	w.SetGeneration(1)
	w.SetUID("work-uid")
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	statuses := []interface{}{}
	for _, entry := range manifests {
		obj := &unstructured.Unstructured{Object: entry.(map[string]interface{})}
		status := map[string]interface{}{"phase": "Bound", "uid": "member-pv", "migrationUID": string(m.UID)}
		if obj.GetKind() == "PersistentVolume" {
			nfs, _, _ := unstructured.NestedMap(obj.Object, "spec", "nfs")
			status["nfs"] = nfs
			status["claimRef"] = map[string]interface{}{"name": "checkpoint", "namespace": m.Namespace, "uid": "member-pvc"}
		} else {
			status["uid"] = "member-pvc"
			status["volumeName"] = w.GetName()
		}
		statuses = append(statuses, map[string]interface{}{"identifier": map[string]interface{}{"group": "", "version": "v1", "kind": obj.GetKind(), "name": obj.GetName(), "namespace": obj.GetNamespace()}, "status": status})
	}
	w.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Applied", "status": "True", "observedGeneration": int64(1)}}, "manifestStatuses": statuses}
	return w
}

func TestStandaloneWorkPlan(t *testing.T) {
	m, md, _ := standaloneFixture()
	original := md.DeepCopy()
	w := boundPair(t, m, md)
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	if len(manifests) != 2 {
		t.Fatal("one pair per mapping required")
	}
	pv, pvc := manifests[0].(map[string]interface{}), manifests[1].(map[string]interface{})
	for _, obj := range []map[string]interface{}{pv, pvc} {
		for key, want := range map[string]string{workNameAnnotation: w.GetName(), workNamespaceAnnotation: w.GetNamespace()} {
			got, _, _ := unstructured.NestedString(obj, "metadata", "annotations", key)
			if got != want {
				t.Fatal("routing missing")
			}
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(obj, "metadata", "uid"); found {
			t.Fatal("source UID leaked")
		}
	}
	volume, _, _ := unstructured.NestedString(pvc, "spec", "volumeName")
	class, found, _ := unstructured.NestedString(pvc, "spec", "storageClassName")
	if volume != w.GetName() || !found || class != "" {
		t.Fatal("PVC not statically pinned")
	}
	if !reflect.DeepEqual(original, md) {
		t.Fatal("source metadata mutated")
	}
	h1, _ := planHash(m, md, []*unstructured.Unstructured{w})
	md.Status.Clusters[0].Volumes[0].PVCSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	other, err := BuildWorks(m, md)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := planHash(m, md, other)
	if h1 == h2 {
		t.Fatal("PVC spec excluded from immutable plan")
	}
}

func TestStandaloneValidation(t *testing.T) {
	for _, name := range []string{"annotation", "explicit", "unreferenced", "shadowed", "renamed", "unfenced", "not-held"} {
		t.Run(name, func(t *testing.T) {
			m, md, sts := standaloneFixture()
			rb := testBinding(true, []string{sourceCluster})
			switch name {
			case "explicit", "shadowed":
				sts.Spec.Template.Annotations = nil
				sts.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "checkpoint"}}}}
				if name == "shadowed" {
					sts.Spec.VolumeClaimTemplates = testStatefulSet().Spec.VolumeClaimTemplates
				}
			case "unreferenced":
				sts.Spec.Template.Annotations = nil
			case "renamed":
				m.Spec.Volumes[0].TargetPVC = "other"
			case "unfenced":
				m.Spec.SourceFenced = false
			case "not-held":
				_ = unstructured.SetNestedField(rb.Object, false, "spec", "suspension", "dispatching")
			}
			r := &PVMigrationReconciler{Client: fakeClient(t, m, md, sts, rb)}
			err := r.validateBinding(context.Background(), m, md)
			if (err == nil) != (name == "annotation" || name == "explicit") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}

func TestStandaloneRejectsUnsafeSourceSpec(t *testing.T) {
	for _, name := range []string{"missing-spec", "no-marker", "template-marker", "renamed", "wrong-pv", "wrong-uid", "oversized", "access-mode", "class", "node-affinity", "duplicate-snapshot"} {
		t.Run(name, func(t *testing.T) {
			m, md, _ := standaloneFixture()
			v := &md.Status.Clusters[0].Volumes[0]
			switch name {
			case "missing-spec":
				v.PVCSpec = nil
			case "no-marker":
				v.Standalone = false
			case "template-marker":
				v.TemplateName = "data"
			case "renamed":
				m.Spec.Volumes[0].TargetPVC = "other"
			case "wrong-pv":
				v.PVCSpec.VolumeName = "wrong"
			case "wrong-uid":
				v.PVSpec.ClaimRef.UID = "wrong"
			case "oversized":
				v.PVCSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("100Gi")
			case "access-mode":
				v.PVCSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			case "class":
				v.PVSpec.StorageClassName = "wrong"
			case "node-affinity":
				v.PVSpec.NodeAffinity = &corev1.VolumeNodeAffinity{}
			case "duplicate-snapshot":
				md.Status.Clusters = append(md.Status.Clusters, md.Status.Clusters[0])
			}
			if _, err := BuildWorks(m, md); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestStandaloneRequiresReciprocalBoundEvidence(t *testing.T) {
	for _, name := range []string{"bound", "pending-pvc", "available-pv", "wrong-volume", "wrong-uid", "wrong-backing", "foreign-owner", "foreign-namespace", "missing", "duplicate", "old-generation", "terminating", "malformed-uid"} {
		t.Run(name, func(t *testing.T) {
			m, md, _ := standaloneFixture()
			w := boundPair(t, m, md)
			s, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
			pv, pvc := s[0].(map[string]interface{}), s[1].(map[string]interface{})
			switch name {
			case "terminating":
				_ = unstructured.SetNestedField(pvc, "2026-11-03T00:00:00Z", "status", "deletionTimestamp")
			case "malformed-uid":
				_ = unstructured.SetNestedField(pvc, map[string]interface{}{"invalid": "uid"}, "status", "uid")
			case "pending-pvc":
				_ = unstructured.SetNestedField(pvc, "Pending", "status", "phase")
			case "available-pv":
				_ = unstructured.SetNestedField(pv, "Available", "status", "phase")
			case "wrong-volume":
				_ = unstructured.SetNestedField(pvc, "wrong", "status", "volumeName")
			case "wrong-uid":
				_ = unstructured.SetNestedField(pv, "wrong", "status", "claimRef", "uid")
			case "wrong-backing":
				_ = unstructured.SetNestedField(pv, "/wrong", "status", "nfs", "path")
			case "foreign-owner":
				_ = unstructured.SetNestedField(pvc, "other", "status", "migrationUID")
			case "foreign-namespace":
				_ = unstructured.SetNestedField(pvc, "other", "identifier", "namespace")
			case "missing":
				s = s[:1]
			case "duplicate":
				s[1] = s[0]
			case "old-generation":
				w.SetGeneration(2)
			}
			_ = unstructured.SetNestedSlice(w.Object, s, "status", "manifestStatuses")
			if workApplied(w) != (name == "bound") {
				t.Fatal("incorrect paired application evidence")
			}
		})
	}
}

func TestStandaloneReconcileAndCleanup(t *testing.T) {
	ctx := context.Background()
	m, md, sts := standaloneFixture()
	c := fakeClient(t, m, md, sts, testBinding(true, []string{sourceCluster}))
	r := &PVMigrationReconciler{Client: c}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, req.NamespacedName, m); err != nil {
		t.Fatal(err)
	}
	if m.Status.Phase != "Preparing" || len(m.Status.Works) != 1 || !m.Status.Works[0].Standalone || m.Status.Works[0].Applied {
		t.Fatalf("bad durable plan: %#v", m.Status)
	}
	w := object(WorkGVK, m.Status.Works[0].Namespace, m.Status.Works[0].Name)
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	w.Object["status"] = boundPair(t, m, md).Object["status"]
	w.SetGeneration(1)
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, m); err != nil {
		t.Fatal(err)
	}
	if m.Status.Phase != "Ready" {
		t.Fatalf("not Ready: %#v", m.Status)
	}
	cleanup := &PVCleanupReconciler{Client: c}
	for i := 0; i < 2; i++ {
		if _, err := cleanup.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, req.NamespacedName, m); err != nil {
		t.Fatal(err)
	}
	if m.Status.Phase != "Completed" || !m.Status.Works[0].Detached {
		t.Fatalf("not detached: %#v", m.Status)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), w); !apierrors.IsNotFound(err) {
		t.Fatalf("Work recreated: %v", err)
	}
}

func TestStandaloneCleanupRefusesMutation(t *testing.T) {
	for _, mutation := range []string{"backing", "removed-pvc", "plan", "binding"} {
		t.Run(mutation, func(t *testing.T) {
			m, md, _ := standaloneFixture()
			w := boundPair(t, m, md)
			hash, err := planHash(m, md, []*unstructured.Unstructured{w})
			if err != nil {
				t.Fatal(err)
			}
			m.Status = api.PVMigrationStatus{Phase: "Ready", ObservedGeneration: 1, PlanHash: hash, Works: []api.WorkReference{{Name: w.GetName(), Namespace: w.GetNamespace(), Standalone: true, Applied: true}}}
			switch mutation {
			case "plan":
				md.Status.Clusters[0].Volumes[0].PVCUID = "replaced"
			case "binding":
				s, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
				_ = unstructured.SetNestedField(s[1].(map[string]interface{}), "wrong", "status", "volumeName")
				_ = unstructured.SetNestedSlice(w.Object, s, "status", "manifestStatuses")
			default:
				manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
				if mutation == "backing" {
					_ = unstructured.SetNestedField(manifests[1].(map[string]interface{}), "wrong", "spec", "volumeName")
				} else {
					manifests = manifests[:1]
					s, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
					_ = unstructured.SetNestedSlice(w.Object, s[:1], "status", "manifestStatuses")
				}
				_ = unstructured.SetNestedSlice(w.Object, manifests, "spec", "workload", "manifests")
			}
			c := fakeClient(t, m, md, w)
			r := &PVCleanupReconciler{Client: c}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err == nil {
				t.Fatal("unsafe detach accepted")
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(w), object(WorkGVK, w.GetNamespace(), w.GetName())); err != nil {
				t.Fatal("conflicting Work deleted")
			}
		})
	}
}
