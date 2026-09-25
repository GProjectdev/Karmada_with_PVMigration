package member

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	migrationv1alpha1 "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
)

const defaultPollInterval = time.Minute

// PVSyncReconciler collects StatefulSet PVC/PV metadata from one member cluster.
type PVSyncReconciler struct {
	client.Client
	ClusterName  string
	PollInterval time.Duration
}

func (r *PVSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pollInterval := r.pollInterval()

	var metadata migrationv1alpha1.PVMetadata
	if err := r.Get(ctx, req.NamespacedName, &metadata); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if metadata.Spec.SourceCluster != r.ClusterName {
		return ctrl.Result{}, nil
	}

	base := metadata.DeepCopy()
	nextStatus := metadata.Status.DeepCopy()
	observed, err := r.collect(ctx, &metadata)
	if err != nil {
		nextStatus = failedStatus(nextStatus, err.Error())
	} else {
		nextStatus = successfulStatus(nextStatus, &metadata, observed)
	}

	if !reflect.DeepEqual(metadata.Status, *nextStatus) {
		metadata.Status = *nextStatus
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, &metadata, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *PVSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("pvmetadata-pvsync").
		For(&migrationv1alpha1.PVMetadata{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

type observation struct {
	WorkloadUID string
	Volumes     []migrationv1alpha1.VolumeMetadata
	CollectedAt *time.Time
}

func (r *PVSyncReconciler) collect(ctx context.Context, metadata *migrationv1alpha1.PVMetadata) (observation, error) {
	var observed observation
	if metadata.Spec.WorkloadRef.Name == "" {
		return observed, fmt.Errorf("workloadRef.name is required")
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: metadata.Namespace, Name: metadata.Spec.WorkloadRef.Name}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return observed, fmt.Errorf("statefulset %s/%s not found", metadata.Namespace, metadata.Spec.WorkloadRef.Name)
		}
		return observed, fmt.Errorf("get statefulset %s/%s: %w", metadata.Namespace, metadata.Spec.WorkloadRef.Name, err)
	}
	observed.WorkloadUID = string(sts.UID)

	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	if replicas <= 0 {
		return observed, fmt.Errorf("statefulset %s/%s has zero replicas", sts.Namespace, sts.Name)
	}
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		return observed, fmt.Errorf("statefulset %s/%s has no volumeClaimTemplates", sts.Namespace, sts.Name)
	}

	startOrdinal := int32(0)
	if sts.Spec.Ordinals != nil {
		startOrdinal = sts.Spec.Ordinals.Start
	}

	volumes := make([]migrationv1alpha1.VolumeMetadata, 0, len(sts.Spec.VolumeClaimTemplates)*int(replicas))
	for _, template := range sts.Spec.VolumeClaimTemplates {
		if template.Name == "" {
			return observed, fmt.Errorf("statefulset %s/%s has a volumeClaimTemplate without a name", sts.Namespace, sts.Name)
		}
		for ordinal := startOrdinal; ordinal < startOrdinal+replicas; ordinal++ {
			pvcName := fmt.Sprintf("%s-%s-%d", template.Name, sts.Name, ordinal)
			volume, err := r.collectVolume(ctx, sts.Namespace, template.Name, ordinal, pvcName)
			if err != nil {
				return observed, err
			}
			volumes = append(volumes, volume)
		}
	}

	now := time.Now()
	observed.Volumes = volumes
	observed.CollectedAt = &now
	return observed, nil
}

func (r *PVSyncReconciler) collectVolume(ctx context.Context, namespace, templateName string, ordinal int32, pvcName string) (migrationv1alpha1.VolumeMetadata, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pvc %s/%s not found", namespace, pvcName)
		}
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("get pvc %s/%s: %w", namespace, pvcName, err)
	}
	if pvc.UID == "" {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pvc %s/%s has empty uid", namespace, pvcName)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pvc %s/%s is not bound", namespace, pvcName)
	}
	if pvc.Spec.VolumeName == "" {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pvc %s/%s is not bound to a pv", namespace, pvcName)
	}

	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pv %s bound by pvc %s/%s not found", pvc.Spec.VolumeName, namespace, pvcName)
		}
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("get pv %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Status.Phase != corev1.VolumeBound {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pv %s is not bound", pv.Name)
	}
	if pv.Spec.ClaimRef == nil {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pv %s has no claimRef", pv.Name)
	}
	if pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.Namespace != pvc.Namespace || pv.Spec.ClaimRef.UID != pvc.UID {
		return migrationv1alpha1.VolumeMetadata{}, fmt.Errorf("pv %s claimRef does not match pvc %s/%s", pv.Name, pvc.Namespace, pvc.Name)
	}

	return migrationv1alpha1.VolumeMetadata{
		PVCName:      pvc.Name,
		PVCUID:       string(pvc.UID),
		TemplateName: templateName,
		Ordinal:      ordinal,
		PVName:       pv.Name,
		PVSpec:       *pv.Spec.DeepCopy(),
	}, nil
}

func (r *PVSyncReconciler) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultPollInterval
}

func successfulStatus(current *migrationv1alpha1.PVMetadataStatus, metadata *migrationv1alpha1.PVMetadata, observed observation) *migrationv1alpha1.PVMetadataStatus {
	current.ObservedGeneration = metadata.Generation
	current.Ready = true
	current.Message = "collected"
	current.CollectedAt = metav1Time(observed.CollectedAt)
	current.WorkloadUID = observed.WorkloadUID
	current.Volumes = cloneVolumes(observed.Volumes)
	return current
}

func failedStatus(current *migrationv1alpha1.PVMetadataStatus, message string) *migrationv1alpha1.PVMetadataStatus {
	current.Ready = false
	current.Message = message
	return current
}

func cloneVolumes(in []migrationv1alpha1.VolumeMetadata) []migrationv1alpha1.VolumeMetadata {
	if in == nil {
		return nil
	}
	out := make([]migrationv1alpha1.VolumeMetadata, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}

func metav1Time(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	return &metav1.Time{Time: *t}
}
