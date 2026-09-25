package management

import (
	"context"
	"fmt"
	"time"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MetadataDiscovery uses only Karmada objects; it never opens a member client.
type MetadataDiscovery struct {
	client.Client
	PollInterval time.Duration
}

func (r *MetadataDiscovery) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	delay := r.PollInterval
	if delay <= 0 {
		delay = 30 * time.Second
	}
	result := ctrl.Result{RequeueAfter: delay}
	rb := object(BindingGVK, req.Namespace, req.Name)
	if err := r.Get(ctx, req.NamespacedName, rb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	kind, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "kind")
	version, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "apiVersion")
	name, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "name")
	if kind != "StatefulSet" || version != "apps/v1" || !rb.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: name}, sts); err != nil {
		return result, client.IgnoreNotFound(err)
	}
	if sts.Labels[OptInLabel] != "enabled" {
		return result, nil
	}
	clusters, _, err := unstructured.NestedSlice(rb.Object, "spec", "clusters")
	if err != nil {
		return result, err
	}
	for _, entry := range clusters {
		cluster, ok := entry.(map[string]interface{})
		if !ok {
			return result, fmt.Errorf("invalid ResourceBinding cluster")
		}
		clusterName, _ := cluster["name"].(string)
		if clusterName == "" {
			continue
		}
		metadataName := stableName("pvm", string(sts.UID), clusterName)
		md := &api.PVMetadata{}
		key := types.NamespacedName{Namespace: sts.Namespace, Name: metadataName}
		if err := r.Get(ctx, key, md); apierrors.IsNotFound(err) {
			md = &api.PVMetadata{ObjectMeta: metav1.ObjectMeta{Name: metadataName, Namespace: sts.Namespace, Labels: map[string]string{ManagedLabel: ManagerName}}}
			md.Spec.WorkloadRef.Name = sts.Name
			md.Spec.WorkloadRef.UID = string(sts.UID)
			md.Spec.SourceCluster = clusterName
			if err := r.Create(ctx, md); err != nil && !apierrors.IsAlreadyExists(err) {
				return result, err
			}
			if err := r.Get(ctx, key, md); err != nil {
				return result, err
			}
		} else if err != nil {
			return result, err
		}
		if md.Spec.SourceCluster != clusterName || md.Spec.WorkloadRef.Name != sts.Name || md.Spec.WorkloadRef.UID != string(sts.UID) || md.Labels[ManagedLabel] != ManagerName {
			return result, fmt.Errorf("PVMetadata %s ownership/spec conflict", metadataName)
		}
		pp := object(PolicyGVK, sts.Namespace, metadataName)
		if err := r.Get(ctx, key, pp); err == nil {
			if !metav1.IsControlledBy(pp, md) {
				return result, fmt.Errorf("PropagationPolicy %s ownership conflict", metadataName)
			}
			// Never retarget an old source: its last observation must survive failover.
			names, _, _ := unstructured.NestedStringSlice(pp.Object, "spec", "placement", "clusterAffinity", "clusterNames")
			if len(names) != 1 || names[0] != clusterName {
				return result, fmt.Errorf("PropagationPolicy %s was retargeted", metadataName)
			}
			continue
		} else if !apierrors.IsNotFound(err) {
			return result, err
		}
		pp.SetLabels(map[string]string{ManagedLabel: ManagerName})
		pp.SetOwnerReferences([]metav1.OwnerReference{controllerOwner("migration.dcnlab.com/v1alpha1", "PVMetadata", md.Name, string(md.UID))})
		pp.Object["spec"] = map[string]interface{}{
			"resourceSelectors": []interface{}{map[string]interface{}{"apiVersion": "migration.dcnlab.com/v1alpha1", "kind": "PVMetadata", "name": md.Name, "namespace": md.Namespace}},
			"placement":         map[string]interface{}{"clusterAffinity": map[string]interface{}{"clusterNames": []interface{}{clusterName}}},
		}
		if err := r.Create(ctx, pp); err != nil && !apierrors.IsAlreadyExists(err) {
			return result, err
		}
	}
	return result, nil
}

func (r *MetadataDiscovery) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("pv-metadata-discovery").For(object(BindingGVK, "", "")).Complete(r)
}
