package interpreter_test

import (
	"path/filepath"
	"testing"
)

func TestStandaloneBindingReflection(t *testing.T) {
	for _, kind := range []string{"persistentvolume", "persistentvolumeclaim"} {
		t.Run(kind, func(t *testing.T) {
			filename := kind + "_resource_interpreter.yaml"
			L := loadNamedRICFunction(t, filename, "statusReflection", "ReflectStatus")
			defer L.Close()
			got := callLua(t, L, "ReflectStatus", map[string]any{
				"metadata": map[string]any{"uid": "member-uid", "labels": map[string]any{"migration.dcnlab.com/migration-uid": "migration-uid"}},
				"spec":     map[string]any{"volumeName": "pv", "claimRef": map[string]any{"name": "claim", "namespace": "ns", "uid": "pvc-uid"}, "nfs": map[string]any{"server": "10.0.0.7", "path": "/exports/data"}},
				"status":   map[string]any{"phase": "Bound"},
			}).(map[string]any)
			assertEqual(t, got["uid"], "member-uid", "UID")
			assertEqual(t, got["migrationUID"], "migration-uid", "owner")
			assertEqual(t, got["phase"], "Bound", "phase")
			if kind == "persistentvolume" {
				assertEqual(t, requireMap(t, got, "claimRef")["uid"], "pvc-uid", "binding UID")
				assertEqual(t, requireMap(t, got, "nfs")["path"], "/exports/data", "NFS path")
			} else {
				assertEqual(t, got["volumeName"], "pv", "volumeName")
			}
			root := filepath.Join("..", "..", "config", "karmada", "ric")
			installed := false
			for _, resource := range requireSlice(t, loadYAML(t, filepath.Join(root, "kustomization.yaml")), "resources") {
				installed = installed || resource == filename
			}
			if !installed {
				t.Fatal("RIC not installed")
			}
		})
	}
}

func TestStandaloneMetadataSurvivesAggregationAndSchema(t *testing.T) {
	L := loadRICFunction(t, "statusAggregation", "AggregateStatus")
	defer L.Close()
	got := callLua(t, L, "AggregateStatus", map[string]any{
		"metadata": map[string]any{"generation": 1}, "spec": map[string]any{"sourceCluster": "source"},
	}, []any{map[string]any{"clusterName": "source", "status": map[string]any{
		"observedGeneration": 1, "ready": true, "volumes": []any{map[string]any{
			"pvcName": "checkpoint", "standalone": true, "pvcSpec": map[string]any{"volumeName": "source-pv"},
		}},
	}}}).(map[string]any)
	cluster := requireSlice(t, requireMap(t, got, "status"), "clusters")[0].(map[string]any)
	volume := requireSlice(t, cluster, "volumes")[0].(map[string]any)
	assertEqual(t, volume["standalone"], true, "standalone marker")
	assertEqual(t, requireMap(t, volume, "pvcSpec")["volumeName"], "source-pv", "source PVC spec")
	for _, file := range []string{"migration.dcnlab.com_pvmetadata.yaml", "migration.dcnlab.com_pvmigrations.yaml"} {
		crd := loadYAML(t, filepath.Join("..", "..", "config", "crd", "bases", file))
		version := requireSlice(t, requireMap(t, crd, "spec"), "versions")[0].(map[string]any)
		schema := requireMap(t, requireMap(t, version, "schema"), "openAPIV3Schema")
		status := requireMap(t, requireMap(t, requireMap(t, schema, "properties"), "status"), "properties")
		if file == "migration.dcnlab.com_pvmigrations.yaml" {
			props := requireMap(t, requireMap(t, requireMap(t, status, "works"), "items"), "properties")
			assertEqual(t, requireMap(t, props, "standalone")["type"], "boolean", "durable marker schema")
			assertEqual(t, requireMap(t, props, "pvUID")["type"], "string", "durable PV UID schema")
			assertEqual(t, requireMap(t, props, "pvcUID")["type"], "string", "durable PVC UID schema")
			continue
		}
		clusters := requireMap(t, requireMap(t, requireMap(t, status, "clusters"), "items"), "properties")
		for _, parent := range []map[string]any{status, clusters} {
			props := requireMap(t, requireMap(t, requireMap(t, parent, "volumes"), "items"), "properties")
			assertEqual(t, requireMap(t, props, "standalone")["type"], "boolean", "volume marker schema")
			assertEqual(t, requireMap(t, props, "pvcSpec")["x-kubernetes-preserve-unknown-fields"], true, "PVC spec preservation")
		}
	}
}
