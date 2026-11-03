/*
Copyright 2026 The Karmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package status

import (
	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	"reflect"
	"testing"
)

func TestMultiManifestReflection(t *testing.T) {
	c := &WorkStatusController{}
	pv := workv1alpha1.ManifestStatus{Identifier: workv1alpha1.ResourceIdentifier{Ordinal: 0, Version: "v1", Kind: "PersistentVolume", Name: "pv"}, Health: "Healthy"}
	pvc := workv1alpha1.ManifestStatus{Identifier: workv1alpha1.ResourceIdentifier{Ordinal: 1, Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "training", Name: "pvc"}, Health: "Healthy"}
	for _, order := range [][]workv1alpha1.ManifestStatus{{pv, pvc, pv}, {pvc, pv, pvc}} {
		var got []workv1alpha1.ManifestStatus
		for _, next := range order {
			got = c.mergeStatus(got, next)
		}
		if len(got) != 2 {
			t.Fatalf("PV/PVC reflection lost: %#v", got)
		}
		found := map[string]bool{}
		for _, s := range got {
			found[s.Identifier.Kind] = true
		}
		if !found["PersistentVolume"] || !found["PersistentVolumeClaim"] {
			t.Fatal("missing pair")
		}
	}
	t.Run("update without mutating input", func(t *testing.T) {
		before := []workv1alpha1.ManifestStatus{pv, pvc}
		original := append([]workv1alpha1.ManifestStatus(nil), before...)
		updated := pv
		updated.Health = "Unhealthy"
		got := c.mergeStatus(before, updated)
		if !reflect.DeepEqual(before, original) {
			t.Fatal("input mutated")
		}
		if len(got) != 2 || got[0].Health != "Unhealthy" || !reflect.DeepEqual(got[1], pvc) {
			t.Fatal("update lost peer")
		}
	})
	t.Run("replace obsolete ordinal", func(t *testing.T) {
		replacement := pv
		replacement.Identifier.Name = "replacement"
		got := c.mergeStatus([]workv1alpha1.ManifestStatus{pv, pvc}, replacement)
		if len(got) != 2 || got[0].Identifier.Name != "replacement" {
			t.Fatal("stale ordinal retained")
		}
	})
	t.Run("deduplicate updated ordinal", func(t *testing.T) {
		got := c.mergeStatus([]workv1alpha1.ManifestStatus{pv, pvc, pv}, pv)
		if !reflect.DeepEqual(got, []workv1alpha1.ManifestStatus{pv, pvc}) {
			t.Fatalf("duplicate ordinal retained: %#v", got)
		}
	})
}
