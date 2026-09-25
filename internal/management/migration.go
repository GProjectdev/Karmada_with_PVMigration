package management

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type PVMigrationReconciler struct {
	client.Client
	PollInterval time.Duration
}

func delay(d time.Duration) ctrl.Result {
	if d <= 0 {
		d = 10 * time.Second
	}
	return ctrl.Result{RequeueAfter: d}
}

func setPhase(ctx context.Context, c client.Client, m *api.PVMigration, phase, message string) error {
	base := m.DeepCopy()
	m.Status.Phase, m.Status.Message, m.Status.ObservedGeneration = phase, message, m.Generation
	if reflect.DeepEqual(base.Status, m.Status) {
		return nil
	}
	return c.Status().Patch(ctx, m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *PVMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	m := &api.PVMigration{}
	if err := r.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() || m.Status.Phase == "Completed" || m.Status.Phase == "Ready" {
		return ctrl.Result{}, nil
	}
	result := delay(r.PollInterval)
	md := &api.PVMetadata{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Spec.MetadataRef}, md); err != nil {
		return result, err
	}
	if err := r.validateBinding(ctx, m, md); err != nil {
		return result, setPhase(ctx, r.Client, m, "Pending", err.Error())
	}
	works, err := BuildWorks(m, md)
	if err != nil {
		return result, setPhase(ctx, r.Client, m, "Pending", err.Error())
	}
	// Persist identities before creation/deletion, so a restart never recreates a detached Work.
	hash, err := planHash(m, md, works)
	if err != nil {
		return result, err
	}
	if len(m.Status.Works) == 0 {
		base := m.DeepCopy()
		m.Status.PlanHash = hash
		for _, w := range works {
			m.Status.Works = append(m.Status.Works, api.WorkReference{Name: w.GetName(), Namespace: w.GetNamespace()})
		}
		m.Status.Phase, m.Status.Message, m.Status.ObservedGeneration = "Preparing", "PV Work identities recorded", m.Generation
		if err := r.Status().Patch(ctx, m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return result, err
		}
		return result, nil
	}
	if m.Status.PlanHash != hash {
		return result, setPhase(ctx, r.Client, m, "Pending", "source snapshot identities/specs changed after planning; inspect existing Works before starting a new request")
	}
	if len(m.Status.Works) != len(works) {
		return result, setPhase(ctx, r.Client, m, "Failed", "immutable migration plan differs from recorded Works")
	}
	base := m.DeepCopy()
	all := true
	for i, expected := range works {
		ref := &m.Status.Works[i]
		if ref.Name != expected.GetName() || ref.Namespace != expected.GetNamespace() {
			return result, fmt.Errorf("migration Work identity changed")
		}
		if ref.Detached {
			continue
		}
		actual := object(WorkGVK, ref.Namespace, ref.Name)
		err := r.Get(ctx, client.ObjectKeyFromObject(actual), actual)
		if apierrors.IsNotFound(err) {
			if ref.Applied {
				return result, fmt.Errorf("applied Work %s disappeared before cleanup; inspect retained PV before recovery", ref.Name)
			}
			if err := r.Create(ctx, expected); err != nil && !apierrors.IsAlreadyExists(err) {
				return result, err
			}
			all = false
			continue
		}
		if err != nil {
			return result, err
		}
		if err := validateWork(actual, expected, m); err != nil {
			return result, err
		}
		ref.Applied = workApplied(actual)
		if !ref.Applied {
			all = false
		}
	}
	m.Status.Phase = "Preparing"
	m.Status.Message = "Waiting for Applied Work and reflected Available/Bound PV"
	if all {
		m.Status.Phase = "Ready"
		m.Status.Message = "PV application confirmed; cleanup may detach Works"
	}
	m.Status.ObservedGeneration = m.Generation
	if !reflect.DeepEqual(base.Status, m.Status) {
		if err := r.Status().Patch(ctx, m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (r *PVMigrationReconciler) validateBinding(ctx context.Context, m *api.PVMigration, md *api.PVMetadata) error {
	if !m.Spec.SourceFenced {
		return fmt.Errorf("sourceFenced must attest that the source workload can no longer write")
	}
	if m.Spec.SourceCluster == m.Spec.TargetCluster || m.Spec.SourceCluster == "" || m.Spec.TargetCluster == "" {
		return fmt.Errorf("different source and target clusters are required")
	}
	if md.Spec.SourceCluster != m.Spec.SourceCluster {
		return fmt.Errorf("metadata belongs to a different source cluster")
	}
	rb := object(BindingGVK, m.Namespace, m.Spec.ResourceBinding)
	if err := r.Get(ctx, client.ObjectKeyFromObject(rb), rb); err != nil {
		return err
	}
	kind, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "kind")
	name, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "name")
	version, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "apiVersion")
	if kind != "StatefulSet" || version != "apps/v1" || name != md.Spec.WorkloadRef.Name {
		return fmt.Errorf("ResourceBinding does not match PVMetadata StatefulSet")
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, sts); err != nil {
		return err
	}
	if md.Spec.WorkloadRef.UID == "" || md.Spec.WorkloadRef.UID != string(sts.UID) {
		return fmt.Errorf("PVMetadata belongs to an old or unspecified StatefulSet UID")
	}
	suspended, _, _ := unstructured.NestedBool(rb.Object, "spec", "suspension", "dispatching")
	if !suspended {
		return fmt.Errorf("suspend ResourceBinding dispatching before requesting PV migration")
	}
	var peers api.PVMigrationList
	if err := r.List(ctx, &peers, client.InNamespace(m.Namespace)); err != nil {
		return err
	}
	for _, peer := range peers.Items {
		if peer.UID == m.UID || peer.Spec.TargetCluster != m.Spec.TargetCluster {
			continue
		}
		reserved := peer.Status.Phase == "Ready" || peer.Status.Phase == "Completed"
		for _, ref := range peer.Status.Works {
			reserved = reserved || ref.Applied || ref.Detached
		}
		older := peer.CreationTimestamp.Time.Before(m.CreationTimestamp.Time) || (peer.CreationTimestamp.Time.Equal(m.CreationTimestamp.Time) && string(peer.UID) < string(m.UID))
		if !reserved && !older {
			continue
		}
		for _, existing := range peer.Spec.Volumes {
			for _, requested := range m.Spec.Volumes {
				if existing.TargetPVC == requested.TargetPVC {
					return fmt.Errorf("target PVC %s already reserved by PVMigration %s; retain its record until the PV is deliberately retired", requested.TargetPVC, peer.Name)
				}
			}
		}
	}
	clusters, _, _ := unstructured.NestedSlice(rb.Object, "spec", "clusters")
	for _, item := range clusters {
		c, ok := item.(map[string]interface{})
		if ok && c["name"] == m.Spec.TargetCluster {
			return fmt.Errorf("target is already selected by ResourceBinding; pre-stage PVs before changing placement")
		}
	}
	// Validate target PVC names against the source template: prevent arbitrary claim rebinding.
	byOrdinal := map[string]map[string]bool{}
	for _, mapping := range m.Spec.Volumes {
		valid := false
		for _, template := range sts.Spec.VolumeClaimTemplates {
			prefix := template.Name + "-" + sts.Name + "-"
			if validOrdinalPVC(mapping.TargetPVC, prefix) {
				ordinal := strings.TrimPrefix(mapping.TargetPVC, prefix)
				if byOrdinal[ordinal] == nil {
					byOrdinal[ordinal] = map[string]bool{}
				}
				byOrdinal[ordinal][template.Name] = true
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("target PVC %s is not a StatefulSet claim-template ordinal", mapping.TargetPVC)
		}
	}
	for ordinal, templates := range byOrdinal {
		if len(templates) != len(sts.Spec.VolumeClaimTemplates) {
			return fmt.Errorf("target ordinal %s must map every volumeClaimTemplate", ordinal)
		}
	}
	return nil
}

func validOrdinalPVC(name, prefix string) bool {
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return false
	}
	suffix := name[len(prefix):]
	if len(suffix) > 1 && suffix[0] == '0' {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

// BuildWorks reconstitutes only shared NFS volumes. It does not copy storage bytes.
func BuildWorks(m *api.PVMigration, md *api.PVMetadata) ([]*unstructured.Unstructured, error) {
	if len(m.Spec.Volumes) == 0 {
		return nil, fmt.Errorf("at least one explicit PVC mapping is required")
	}
	if len(validation.IsDNS1123Subdomain(m.Spec.TargetCluster)) > 0 || len("karmada-es-"+m.Spec.TargetCluster) > 63 {
		return nil, fmt.Errorf("invalid target execution namespace")
	}
	var snapshot *api.ClusterSnapshot
	for i := range md.Status.Clusters {
		if md.Status.Clusters[i].ClusterName == m.Spec.SourceCluster {
			snapshot = &md.Status.Clusters[i]
			break
		}
	}
	if snapshot == nil || snapshot.CollectedAt == nil || snapshot.ObservedGeneration != md.Generation || snapshot.WorkloadUID == "" {
		return nil, fmt.Errorf("no matching collected source snapshot; wait for ResourceInterpreterCustomization aggregation")
	}
	volumes := map[string]api.VolumeMetadata{}
	for _, v := range snapshot.Volumes {
		if _, exists := volumes[v.PVCName]; exists {
			return nil, fmt.Errorf("duplicate snapshot PVC %s", v.PVCName)
		}
		volumes[v.PVCName] = v
	}
	seenSource, seenTarget := map[string]bool{}, map[string]bool{}
	sourceOrdinalByTarget := map[string]int32{}
	works := []*unstructured.Unstructured{}
	for _, mapping := range m.Spec.Volumes {
		if seenSource[mapping.SourcePVC] || seenTarget[mapping.TargetPVC] {
			return nil, fmt.Errorf("PVC mapping must be one-to-one")
		}
		seenSource[mapping.SourcePVC], seenTarget[mapping.TargetPVC] = true, true
		v, ok := volumes[mapping.SourcePVC]
		if !ok {
			return nil, fmt.Errorf("source PVC %s not present in snapshot", mapping.SourcePVC)
		}
		prefix := v.TemplateName + "-" + md.Spec.WorkloadRef.Name + "-"
		if v.TemplateName == "" || !validOrdinalPVC(mapping.TargetPVC, prefix) {
			return nil, fmt.Errorf("source and target PVC must use the same claim template")
		}
		targetOrdinal := strings.TrimPrefix(mapping.TargetPVC, prefix)
		if prior, exists := sourceOrdinalByTarget[targetOrdinal]; exists && prior != v.Ordinal {
			return nil, fmt.Errorf("all volumes for a target ordinal must come from one source ordinal")
		}
		sourceOrdinalByTarget[targetOrdinal] = v.Ordinal
		if len(validation.IsDNS1123Subdomain(mapping.TargetPVC)) > 0 {
			return nil, fmt.Errorf("invalid target PVC name")
		}
		spec := v.PVSpec.DeepCopy()
		if spec.NFS == nil || spec.NFS.Server == "" || spec.NFS.Path == "" || spec.NodeAffinity != nil {
			return nil, fmt.Errorf("PVC %s requires portable NFS without node affinity; CSI/EBS/local sources are unsupported", v.PVCName)
		}
		if spec.ClaimRef == nil || spec.ClaimRef.Namespace != m.Namespace || spec.ClaimRef.Name != v.PVCName || string(spec.ClaimRef.UID) != v.PVCUID || v.PVCUID == "" {
			return nil, fmt.Errorf("source PV/PVC binding identity mismatch")
		}
		// Copy only the supported source, never provisioner finalizers or source-cluster UIDs.
		nfs := spec.NFS.DeepCopy()
		if !reflect.DeepEqual(spec.PersistentVolumeSource, corev1.PersistentVolumeSource{NFS: nfs}) {
			return nil, fmt.Errorf("ambiguous PV volume sources")
		}
		spec.PersistentVolumeSource = corev1.PersistentVolumeSource{NFS: nfs}
		spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		spec.ClaimRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: m.Namespace, Name: mapping.TargetPVC}
		pvName := stableName("pvm", md.Spec.WorkloadRef.UID, m.Spec.TargetCluster, m.Namespace, mapping.TargetPVC)
		pv := &corev1.PersistentVolume{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"}, ObjectMeta: metav1.ObjectMeta{Name: pvName, Labels: map[string]string{ManagedLabel: ManagerName, OwnerLabel: string(m.UID)}}, Spec: *spec}
		manifest, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pv)
		if err != nil {
			return nil, err
		}
		delete(manifest, "status")
		w := object(WorkGVK, "karmada-es-"+m.Spec.TargetCluster, pvName)
		w.SetLabels(map[string]string{ManagedLabel: ManagerName, OwnerLabel: string(m.UID)})
		// No cross-namespace ownerReference: cleanup is explicit and deletion always preserves PVs.
		w.Object["spec"] = map[string]interface{}{"preserveResourcesOnDeletion": true, "workload": map[string]interface{}{"manifests": []interface{}{manifest}}}
		works = append(works, w)
	}
	return works, nil
}

func validateWork(actual, expected *unstructured.Unstructured, m *api.PVMigration) error {
	if actual.GetLabels()[OwnerLabel] != string(m.UID) || actual.GetLabels()[ManagedLabel] != ManagerName || !reflect.DeepEqual(actual.Object["spec"], expected.Object["spec"]) {
		return fmt.Errorf("Work %s ownership or immutable spec conflict", actual.GetName())
	}
	if actual.GetGeneration() > 1 {
		return fmt.Errorf("Work %s was modified; refuse stale application evidence", actual.GetName())
	}
	return nil
}

func workApplied(w *unstructured.Unstructured) bool {
	if !w.GetDeletionTimestamp().IsZero() {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
	applied := false
	for _, x := range conditions {
		c, ok := x.(map[string]interface{})
		if !ok {
			continue
		}
		if c["type"] == "Applied" && c["status"] == "True" {
			generation, found, _ := unstructured.NestedInt64(c, "observedGeneration")
			if found && generation != 0 && generation != w.GetGeneration() {
				return false
			}
			applied = true
		}
	}
	if !applied {
		return false
	}
	statuses, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	if len(statuses) != 1 || len(manifests) != 1 {
		return false
	}
	s, ok := statuses[0].(map[string]interface{})
	if !ok {
		return false
	}
	manifest, ok := manifests[0].(map[string]interface{})
	if !ok {
		return false
	}
	name, _, _ := unstructured.NestedString(manifest, "metadata", "name")
	sname, _, _ := unstructured.NestedString(s, "identifier", "name")
	kind, _, _ := unstructured.NestedString(s, "identifier", "kind")
	phase, _, _ := unstructured.NestedString(s, "status", "phase")
	return name == sname && kind == "PersistentVolume" && (phase == "Available" || phase == "Bound")
}

func (r *PVMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("pv-migration").For(&api.PVMigration{}).Complete(r)
}
