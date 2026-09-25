package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	OptInLabel   = "migration.dcnlab.com/pv-metadata"
	OwnerLabel   = "migration.dcnlab.com/migration-uid"
	ManagedLabel = "migration.dcnlab.com/managed-by"
	ManagerName  = "pv-migration-system"
)

var (
	BindingGVK = schema.GroupVersionKind{Group: "work.karmada.io", Version: "v1alpha2", Kind: "ResourceBinding"}
	WorkGVK    = schema.GroupVersionKind{Group: "work.karmada.io", Version: "v1alpha1", Kind: "Work"}
	PolicyGVK  = schema.GroupVersionKind{Group: "policy.karmada.io", Version: "v1alpha1", Kind: "PropagationPolicy"}
)

func object(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func list(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, opts ...client.ListOption) ([]unstructured.Unstructured, error) {
	u := &unstructured.UnstructuredList{}
	u.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(ctx, u, opts...); err != nil {
		return nil, err
	}
	return u.Items, nil
}

func stableName(prefix string, parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		fmt.Fprintf(h, "%d:%s;", len(s), s)
	}
	return prefix + "-" + hex.EncodeToString(h.Sum(nil))[:24]
}

func controllerOwner(apiVersion, kind, name, uid string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid), Controller: &controller}
}
