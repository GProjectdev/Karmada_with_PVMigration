package interpreter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestPVMetadataRICHealthUsesReadyAndObservedGeneration(t *testing.T) {
	L := loadRICFunction(t, "healthInterpretation", "InterpretHealth")
	defer L.Close()

	tests := []struct {
		name     string
		observed map[string]any
		want     bool
	}{
		{name: "nil status", observed: map[string]any{"metadata": map[string]any{"generation": 3}}, want: false},
		{name: "ready current generation", observed: map[string]any{"metadata": map[string]any{"generation": 3}, "status": map[string]any{"ready": true, "observedGeneration": 3}}, want: true},
		{name: "ready stale generation", observed: map[string]any{"metadata": map[string]any{"generation": 4}, "status": map[string]any{"ready": true, "observedGeneration": 3}}, want: false},
		{name: "not ready current generation", observed: map[string]any{"metadata": map[string]any{"generation": 3}, "status": map[string]any{"ready": false, "observedGeneration": 3}}, want: false},
		{name: "missing generation uses ready", observed: map[string]any{"status": map[string]any{"ready": true}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callLua(t, L, "InterpretHealth", tt.observed).(bool)
			if got != tt.want {
				t.Fatalf("InterpretHealth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPVMetadataRICReflectionDoesNotRecurseClusterAggregation(t *testing.T) {
	L := loadRICFunction(t, "statusReflection", "ReflectStatus")
	defer L.Close()

	got := callLua(t, L, "ReflectStatus", map[string]any{"status": map[string]any{
		"observedGeneration": 7,
		"ready":              true,
		"message":            "fresh",
		"collectedAt":        "2026-09-25T00:00:00Z",
		"workloadUID":        "workload-a",
		"volumes":            []any{volume("data-0", 0)},
		"clusters":           []any{map[string]any{"clusterName": "other", "ready": true}},
	}}).(map[string]any)

	if _, ok := got["clusters"]; ok {
		t.Fatalf("ReflectStatus recursed status.clusters into reflected status: %#v", got["clusters"])
	}
	assertEqual(t, got["observedGeneration"], float64(7), "observedGeneration")
	assertEqual(t, got["ready"], true, "ready")
	assertEqual(t, got["workloadUID"], "workload-a", "workloadUID")
}

func TestPVMetadataRICAggregationReturnsWholeDesiredObjectAndStableSorts(t *testing.T) {
	L := loadRICFunction(t, "statusAggregation", "AggregateStatus")
	defer L.Close()
	desired := baseDesired()
	setSourceCluster(desired, "cluster-a")

	got := callLua(t, L, "AggregateStatus", desired, []any{
		statusItem("cluster-b", true, 5, "2026-09-25T00:02:00Z", "uid-b", []any{volume("data-b-1", 1), volume("data-b-0", 0)}),
		statusItem("cluster-a", true, 5, "2026-09-25T00:01:00Z", "uid-a", []any{volume("data-a-1", 1), volume("data-a-0", 0)}),
	}).(map[string]any)

	if got["kind"] != "PVMetadata" {
		t.Fatalf("AggregateStatus must return the whole desired object, got keys %v", sortedKeys(got))
	}
	clusters := requireSlice(t, requireMap(t, got, "status"), "clusters")
	requireClusterOrder(t, clusters, "cluster-a")
	requireVolumeOrder(t, requireMapAt(t, clusters, 0), "data-a-0", "data-a-1")
}

func TestPVMetadataRICAggregationRetainsLastGoodOnMissingAndUnsuccessfulObservations(t *testing.T) {
	L := loadRICFunction(t, "statusAggregation", "AggregateStatus")
	defer L.Close()

	desired := baseDesired()
	setSourceCluster(desired, "cluster-a")
	desired["status"] = map[string]any{"clusters": []any{
		map[string]any{"clusterName": "cluster-a", "ready": true, "observedGeneration": 4, "collectedAt": "2026-09-25T00:00:00Z", "workloadUID": "old-uid-a", "volumes": []any{volume("last-good-a-0", 0)}},
		map[string]any{"clusterName": "cluster-b", "ready": true, "observedGeneration": 4, "collectedAt": "2026-09-25T00:00:10Z", "workloadUID": "old-uid-b", "volumes": []any{volume("last-good-b-0", 0)}},
	}}

	got := callLua(t, L, "AggregateStatus", desired, []any{
		statusItem("cluster-a", false, 5, "2026-09-25T00:05:00Z", "bad-new-uid", []any{volume("poison-a-0", 0)}),
	}).(map[string]any)
	clusters := requireSlice(t, requireMap(t, got, "status"), "clusters")
	requireClusterOrder(t, clusters, "cluster-a")

	clusterA := requireMapAt(t, clusters, 0)
	assertEqual(t, clusterA["ready"], false, "cluster-a ready")
	assertEqual(t, clusterA["observedGeneration"], float64(4), "cluster-a observedGeneration")
	assertEqual(t, clusterA["collectedAt"], "2026-09-25T00:00:00Z", "cluster-a collectedAt")
	assertEqual(t, clusterA["workloadUID"], "old-uid-a", "cluster-a workloadUID")
	requireVolumeOrder(t, clusterA, "last-good-a-0")

	got = callLua(t, L, "AggregateStatus", desired, []any{
		statusItem("cluster-b", true, 5, "2026-09-25T00:06:00Z", "poison-uid", []any{volume("poison-b-0", 0)}),
	}).(map[string]any)
	clusters = requireSlice(t, requireMap(t, got, "status"), "clusters")
	requireClusterOrder(t, clusters, "cluster-a")
	clusterA = requireMapAt(t, clusters, 0)
	assertEqual(t, clusterA["ready"], false, "missing source ready")
	assertEqual(t, clusterA["observedGeneration"], float64(4), "missing source observedGeneration")
	assertEqual(t, clusterA["collectedAt"], "2026-09-25T00:00:00Z", "missing source collectedAt")
	assertEqual(t, clusterA["workloadUID"], "old-uid-a", "missing source workloadUID")
	requireVolumeOrder(t, clusterA, "last-good-a-0")
}

func TestPVMetadataRICAggregationSuccessfulObservationReplacesLastGood(t *testing.T) {
	L := loadRICFunction(t, "statusAggregation", "AggregateStatus")
	defer L.Close()

	desired := baseDesired()
	setSourceCluster(desired, "cluster-a")
	desired["status"] = map[string]any{"clusters": []any{
		map[string]any{"clusterName": "cluster-a", "ready": true, "observedGeneration": 4, "collectedAt": "2026-09-25T00:00:00Z", "workloadUID": "old-uid", "volumes": []any{volume("old-data-0", 0)}},
	}}

	got := callLua(t, L, "AggregateStatus", desired, []any{
		statusItem("cluster-a", true, 5, "2026-09-25T00:10:00Z", "new-uid", []any{volume("new-data-1", 1), volume("new-data-0", 0)}),
	}).(map[string]any)
	clusterA := requireMapAt(t, requireSlice(t, requireMap(t, got, "status"), "clusters"), 0)
	assertEqual(t, clusterA["ready"], true, "ready")
	assertEqual(t, clusterA["observedGeneration"], float64(5), "observedGeneration")
	assertEqual(t, clusterA["collectedAt"], "2026-09-25T00:10:00Z", "collectedAt")
	assertEqual(t, clusterA["workloadUID"], "new-uid", "workloadUID")
	requireVolumeOrder(t, clusterA, "new-data-0", "new-data-1")
}

func TestPVMetadataRICAggregationFiltersSourceClusterAndToleratesNilStatuses(t *testing.T) {
	L := loadRICFunction(t, "statusAggregation", "AggregateStatus")
	defer L.Close()

	desired := baseDesired()
	desired["status"] = map[string]any{"clusters": []any{
		map[string]any{"clusterName": "source", "ready": true, "observedGeneration": 4, "collectedAt": "2026-09-25T00:00:00Z", "workloadUID": "old-source-uid", "volumes": []any{volume("source-last-good", 0)}},
	}}

	got := callLua(t, L, "AggregateStatus", desired, []any{
		map[string]any{"clusterName": "source"},
		statusItem("other", true, 5, "2026-09-25T00:20:00Z", "poison-uid", []any{volume("poison-volume", 0)}),
	}).(map[string]any)

	clusters := requireSlice(t, requireMap(t, got, "status"), "clusters")
	requireClusterOrder(t, clusters, "source")
	source := requireMapAt(t, clusters, 0)
	assertEqual(t, source["ready"], false, "source ready")
	assertEqual(t, source["workloadUID"], "old-source-uid", "source workloadUID")
	requireVolumeOrder(t, source, "source-last-good")
}

func TestCRDsPassKubernetesStructuralAndCELValidation(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "crd", "bases", "migration.dcnlab.com_pvmetadata.yaml"),
		filepath.Join("..", "..", "config", "crd", "bases", "migration.dcnlab.com_pvmigrations.yaml"),
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			crd := loadInternalCRD(t, path)
			if crd.Spec.Group != "migration.dcnlab.com" {
				t.Fatalf("group = %q", crd.Spec.Group)
			}
			if crd.Spec.Names.Plural == "pvmetadatas" {
				t.Fatalf("plural must stay pvmetadata, got %q", crd.Spec.Names.Plural)
			}
			if crd.Spec.Names.Kind == "PVMetadata" && crd.Spec.Names.Plural != "pvmetadata" {
				t.Fatalf("PVMetadata plural = %q, want pvmetadata", crd.Spec.Names.Plural)
			}
			if errs := apivalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) > 0 {
				t.Fatalf("CRD failed Kubernetes validation:\n%s", formatFieldErrors(errs))
			}
		})
	}
}

func TestPVMigrationCRDUsesSimpleSpecImmutabilityRule(t *testing.T) {
	crd := loadYAML(t, filepath.Join("..", "..", "config", "crd", "bases", "migration.dcnlab.com_pvmigrations.yaml"))
	version := requireMapAt(t, requireSlice(t, requireMap(t, crd, "spec"), "versions"), 0)
	schema := requireMap(t, requireMap(t, version, "schema"), "openAPIV3Schema")
	specSchema := requireMap(t, requireMap(t, schema, "properties"), "spec")
	validations := requireSlice(t, specSchema, "x-kubernetes-validations")
	for _, validation := range validations {
		if validation.(map[string]any)["rule"] == "self == oldSelf" {
			return
		}
	}
	t.Fatalf("PVMigration CRD should keep spec immutable with self == oldSelf at spec scope, got %#v", validations)
}

func loadRICFunction(t *testing.T, customizationName, functionName string) *lua.LState {
	t.Helper()
	ric := loadYAML(t, filepath.Join("..", "..", "config", "karmada", "ric", "pvmetadata_resource_interpreter.yaml"))
	customization := requireMap(t, requireMap(t, requireMap(t, ric, "spec"), "customizations"), customizationName)
	script, ok := customization["luaScript"].(string)
	if !ok || script == "" {
		t.Fatalf("missing %s luaScript", customizationName)
	}
	L := lua.NewState()
	if err := L.DoString(script); err != nil {
		L.Close()
		t.Fatalf("loading %s luaScript: %v", customizationName, err)
	}
	if fn := L.GetGlobal(functionName); fn.Type() != lua.LTFunction {
		L.Close()
		t.Fatalf("%s luaScript did not define %s", customizationName, functionName)
	}
	return L
}

func callLua(t *testing.T, L *lua.LState, functionName string, args ...any) any {
	t.Helper()
	fn := L.GetGlobal(functionName)
	largs := make([]lua.LValue, 0, len(args))
	for _, arg := range args {
		largs = append(largs, toLuaValue(L, arg))
	}
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, largs...); err != nil {
		t.Fatalf("calling %s: %v", functionName, err)
	}
	ret := L.Get(-1)
	L.Pop(1)
	return fromLuaValue(ret)
}

func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatalf("converting %s to JSON: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(jsonData, &out); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return out
}

func loadInternalCRD(t *testing.T, path string) *apiextensions.CustomResourceDefinition {
	t.Helper()
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	if err := apixv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apiextensions/v1: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	external, ok := obj.(*apixv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("decoded %s as %T", path, obj)
	}
	// Mirror API defaulting and storage-version initialization on CRD creation.
	scheme.Default(external)
	for _, version := range external.Spec.Versions {
		if version.Storage {
			external.Status.StoredVersions = append(external.Status.StoredVersions, version.Name)
		}
	}
	internal := &apiextensions.CustomResourceDefinition{}
	if err := scheme.Convert(external, internal, nil); err != nil {
		t.Fatalf("convert %s to internal CRD: %v", path, err)
	}
	return internal
}

func baseDesired() map[string]any {
	return map[string]any{
		"apiVersion": "migration.dcnlab.com/v1alpha1",
		"kind":       "PVMetadata",
		"metadata":   map[string]any{"name": "example", "namespace": "default", "generation": 5},
		"spec":       map[string]any{"sourceCluster": "source", "workloadRef": map[string]any{"name": "workload", "uid": "workload-uid"}},
	}
}

func setSourceCluster(desired map[string]any, sourceCluster string) {
	desired["spec"].(map[string]any)["sourceCluster"] = sourceCluster
}

func statusItem(cluster string, ready bool, generation int, collectedAt, workloadUID string, volumes []any) map[string]any {
	return map[string]any{"clusterName": cluster, "status": map[string]any{"ready": ready, "observedGeneration": generation, "message": "observed " + cluster, "collectedAt": collectedAt, "workloadUID": workloadUID, "volumes": volumes}}
}

func volume(pvcName string, ordinal int) map[string]any {
	return map[string]any{"pvcName": pvcName, "ordinal": ordinal, "pvName": "pv-" + pvcName}
}

func toLuaValue(L *lua.LState, value any) lua.LValue {
	switch v := value.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case string:
		return lua.LString(v)
	case int:
		return lua.LNumber(v)
	case float64:
		return lua.LNumber(v)
	case map[string]any:
		table := L.NewTable()
		for key, item := range v {
			table.RawSetString(key, toLuaValue(L, item))
		}
		return table
	case []any:
		table := L.NewTable()
		for _, item := range v {
			table.Append(toLuaValue(L, item))
		}
		return table
	default:
		panic(fmt.Sprintf("unsupported Go value for Lua conversion: %T", value))
	}
}

func fromLuaValue(value lua.LValue) any {
	switch v := value.(type) {
	case lua.LBool:
		return bool(v)
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return float64(v)
	case *lua.LTable:
		return fromLuaTable(v)
	case *lua.LNilType:
		return nil
	default:
		return v.String()
	}
}

func fromLuaTable(table *lua.LTable) any {
	stringsByKey := map[string]any{}
	numbersByKey := map[int]any{}
	table.ForEach(func(key, value lua.LValue) {
		switch k := key.(type) {
		case lua.LString:
			stringsByKey[string(k)] = fromLuaValue(value)
		case lua.LNumber:
			numbersByKey[int(k)] = fromLuaValue(value)
		default:
			stringsByKey[k.String()] = fromLuaValue(value)
		}
	})
	if len(stringsByKey) > 0 {
		for key, value := range numbersByKey {
			stringsByKey[strconv.Itoa(key)] = value
		}
		return stringsByKey
	}
	items := make([]any, 0, len(numbersByKey))
	for i := 1; i <= len(numbersByKey); i++ {
		items = append(items, numbersByKey[i])
	}
	return items
}

func requireMap(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := obj[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, obj[key])
	}
	return value
}

func requireSlice(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	value, ok := obj[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", key, obj[key])
	}
	return value
}

func requireMapAt(t *testing.T, values []any, index int) map[string]any {
	t.Helper()
	if index >= len(values) {
		t.Fatalf("index %d out of range for %#v", index, values)
	}
	value, ok := values[index].(map[string]any)
	if !ok {
		t.Fatalf("item %d = %#v, want object", index, values[index])
	}
	return value
}

func requireClusterOrder(t *testing.T, clusters []any, names ...string) {
	t.Helper()
	if len(clusters) != len(names) {
		t.Fatalf("clusters length = %d, want %d: %#v", len(clusters), len(names), clusters)
	}
	for i, name := range names {
		cluster := requireMapAt(t, clusters, i)
		if cluster["clusterName"] != name {
			t.Fatalf("cluster %d = %q, want %q: %#v", i, cluster["clusterName"], name, clusters)
		}
	}
}

func requireVolumeOrder(t *testing.T, cluster map[string]any, pvcNames ...string) {
	t.Helper()
	volumes := requireSlice(t, cluster, "volumes")
	if len(volumes) != len(pvcNames) {
		t.Fatalf("%s volumes length = %d, want %d: %#v", cluster["clusterName"], len(volumes), len(pvcNames), volumes)
	}
	for i, pvcName := range pvcNames {
		volume := requireMapAt(t, volumes, i)
		if volume["pvcName"] != pvcName {
			t.Fatalf("%s volume %d = %q, want %q: %#v", cluster["clusterName"], i, volume["pvcName"], pvcName, volumes)
		}
	}
}

func assertEqual(t *testing.T, got, want any, label string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
}

func formatFieldErrors(errs field.ErrorList) string {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	return strings.Join(messages, "\n")
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
