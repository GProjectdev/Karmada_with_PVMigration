package interpreter_test

import (
	"path/filepath"
	"testing"
)

func TestPersistentVolumeReflectionReturnsActualPhaseOnly(t *testing.T) {
	L := loadNamedRICFunction(t, "persistentvolume_resource_interpreter.yaml", "statusReflection", "ReflectStatus")
	defer L.Close()
	for _, phase := range []string{"Available", "Bound", "Pending", "Released", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			got := callLua(t, L, "ReflectStatus", map[string]any{
				"metadata": map[string]any{"name": "target-pv"},
				"spec":     map[string]any{"claimRef": map[string]any{"uid": "member-pvc"}},
				"status":   map[string]any{"phase": phase, "message": "not reflected"},
			}).(map[string]any)
			if len(got) != 1 || got["phase"] != phase {
				t.Fatalf("unexpected PV status reflection: %#v", got)
			}
		})
	}
	for _, observed := range []map[string]any{{}, {"status": map[string]any{}}} {
		got := callLua(t, L, "ReflectStatus", observed)
		// The existing Lua test adapter represents an empty table as an empty slice.
		if empty, ok := got.([]any); !ok || len(empty) != 0 {
			t.Fatalf("missing member phase must not invent availability: %#v", got)
		}
	}
}

func TestPersistentVolumeRICIsInstalledForCorePV(t *testing.T) {
	root := filepath.Join("..", "..", "config", "karmada", "ric")
	ric := loadYAML(t, filepath.Join(root, "persistentvolume_resource_interpreter.yaml"))
	target := requireMap(t, requireMap(t, ric, "spec"), "target")
	assertEqual(t, target["apiVersion"], "v1", "target version")
	assertEqual(t, target["kind"], "PersistentVolume", "target kind")
	kustomization := loadYAML(t, filepath.Join(root, "kustomization.yaml"))
	for _, resource := range requireSlice(t, kustomization, "resources") {
		if resource == "persistentvolume_resource_interpreter.yaml" {
			return
		}
	}
	t.Fatal("PV status reflection RIC is not included in deployment")
}
