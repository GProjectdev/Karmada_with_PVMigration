package management

import (
	"context"
	"fmt"
	"reflect"
	"time"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PVCleanupReconciler has no member client and never deletes PVs or PVCs.
type PVCleanupReconciler struct {
	client.Client
	PollInterval time.Duration
}

func (r *PVCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	m := &api.PVMigration{}
	if err := r.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() || m.Status.Phase != "Ready" {
		return ctrl.Result{}, nil
	}
	result := delay(r.PollInterval)
	if m.Status.ObservedGeneration != m.Generation || len(m.Status.Works) == 0 {
		return result, fmt.Errorf("migration has no current durable application evidence")
	}
	base := m.DeepCopy()
	all := true
	for i := range m.Status.Works {
		ref := &m.Status.Works[i]
		if !ref.Applied {
			return result, fmt.Errorf("Work %s not durably applied", ref.Name)
		}
		if ref.Detached {
			continue
		}
		if ref.Namespace != "karmada-es-"+m.Spec.TargetCluster {
			return result, fmt.Errorf("Work namespace ownership mismatch")
		}
		w := object(WorkGVK, ref.Namespace, ref.Name)
		if err := r.Get(ctx, client.ObjectKeyFromObject(w), w); apierrors.IsNotFound(err) {
			ref.Detached = true
			continue
		} else if err != nil {
			return result, err
		}
		preserve, _, _ := unstructured.NestedBool(w.Object, "spec", "preserveResourcesOnDeletion")
		if w.GetLabels()[OwnerLabel] != string(m.UID) || w.GetLabels()[ManagedLabel] != ManagerName || !preserve || w.GetGeneration() > 1 {
			return result, fmt.Errorf("refuse deletion: Work %s ownership/preservation changed", ref.Name)
		}
		all = false
		if !w.GetDeletionTimestamp().IsZero() {
			continue
		}
		if !workApplied(w) {
			return result, fmt.Errorf("Work %s no longer has valid PV application evidence", ref.Name)
		}
		// UID/RV preconditions prevent deleting a replacement or concurrently modified Work.
		uid, rv := w.GetUID(), w.GetResourceVersion()
		if err := r.Delete(ctx, w, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
			return result, err
		}
	}
	if all {
		m.Status.Phase = "Completed"
		m.Status.Message = "All PV Works detached; retained PVs remain. Workload placement/restore is a separate gate."
	}
	if !reflect.DeepEqual(base.Status, m.Status) {
		if err := r.Status().Patch(ctx, m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (r *PVCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("pv-cleanup").For(&api.PVMigration{}).Complete(r)
}
