package management

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "default"
	sourceCluster = "member-a"
	targetCluster = "member-b"
	stsName       = "web"
	pvcName       = "data-web-0"
	targetPVCName = "data-web-1"
	pvcUID        = "pvc-uid-1"
	migrationUID  = "migration-uid-1"
	stsUID        = "sts-uid-1"
)

func TestBuildWorksSanitizesNFSPVAndAllowsLastKnownSnapshot(t *testing.T) {
	m := testMigration()
	md := testMetadata()
	md.Status.Clusters[0].Ready = false

	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatalf("BuildWorks returned error for last-known source snapshot: %v", err)
	}
	if len(works) != 1 {
		t.Fatalf("expected one Work, got %d", len(works))
	}

	w := works[0]
	if got := w.GetNamespace(); got != "karmada-es-"+targetCluster {
		t.Fatalf("unexpected Work namespace %q", got)
	}
	if w.GetLabels()[ManagedLabel] != ManagerName || w.GetLabels()[OwnerLabel] != string(m.UID) {
		t.Fatalf("missing ownership labels: %#v", w.GetLabels())
	}
	preserve, _, _ := unstructured.NestedBool(w.Object, "spec", "preserveResourcesOnDeletion")
	if !preserve {
		t.Fatalf("Work must preserve retained PVs on deletion")
	}

	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	if len(manifests) != 1 {
		t.Fatalf("expected one manifest, got %d", len(manifests))
	}
	manifest, ok := manifests[0].(map[string]interface{})
	if !ok {
		t.Fatalf("manifest is not an object: %#v", manifests[0])
	}
	if _, ok := manifest["status"]; ok {
		t.Fatalf("BuildWorks must not copy PV status")
	}
	if got, _, _ := unstructured.NestedString(manifest, "metadata", "labels", ManagedLabel); got != ManagerName {
		t.Fatalf("PV manifest missing managed label: %q", got)
	}
	if got, _, _ := unstructured.NestedString(manifest, "spec", "persistentVolumeReclaimPolicy"); got != string(corev1.PersistentVolumeReclaimRetain) {
		t.Fatalf("expected Retain reclaim policy, got %q", got)
	}
	if got, _, _ := unstructured.NestedString(manifest, "spec", "claimRef", "name"); got != targetPVCName {
		t.Fatalf("expected target claimRef name %q, got %q", targetPVCName, got)
	}
	if got, _, _ := unstructured.NestedString(manifest, "spec", "claimRef", "uid"); got != "" {
		t.Fatalf("target claimRef must not inherit source UID, got %q", got)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(manifest, "spec", "csi"); found {
		t.Fatalf("PV manifest unexpectedly contains CSI source")
	}
	if got, _, _ := unstructured.NestedString(manifest, "spec", "nfs", "server"); got != "10.0.0.7" {
		t.Fatalf("expected NFS server to survive sanitization, got %q", got)
	}
}

func TestBuildWorksRejectsUnsafeSourcesAndDuplicatePlans(t *testing.T) {
	tests := []struct {
		name string
		edit func(*api.PVMigration, *api.PVMetadata)
		want string
	}{
		{
			name: "unsupported CSI source",
			edit: func(_ *api.PVMigration, md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes[0].PVSpec.PersistentVolumeSource = corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-1"},
				}
			},
			want: "unsupported",
		},
		{
			name: "ambiguous NFS plus hostPath source",
			edit: func(_ *api.PVMigration, md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes[0].PVSpec.PersistentVolumeSource.HostPath = &corev1.HostPathVolumeSource{Path: "/mnt/data"}
			},
			want: "ambiguous PV volume sources",
		},
		{
			name: "node-affine NFS source",
			edit: func(_ *api.PVMigration, md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes[0].PVSpec.NodeAffinity = &corev1.VolumeNodeAffinity{}
			},
			want: "unsupported",
		},
		{
			name: "duplicate source snapshot PVC",
			edit: func(_ *api.PVMigration, md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes = append(md.Status.Clusters[0].Volumes, md.Status.Clusters[0].Volumes[0])
			},
			want: "duplicate snapshot PVC",
		},
		{
			name: "duplicate target mapping",
			edit: func(m *api.PVMigration, _ *api.PVMetadata) {
				m.Spec.Volumes = append(m.Spec.Volumes, api.VolumeMapping{SourcePVC: "data-web-2", TargetPVC: targetPVCName})
			},
			want: "one-to-one",
		},
		{
			name: "missing source snapshot freshness",
			edit: func(_ *api.PVMigration, md *api.PVMetadata) {
				md.Status.Clusters[0].ObservedGeneration = md.Generation - 1
			},
			want: "no matching collected source snapshot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testMigration()
			md := testMetadata()
			tt.edit(m, md)

			_, err := BuildWorks(m, md)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildWorks error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestBuildWorksRejectsTargetTemplateMismatch(t *testing.T) {
	m := testMigration()
	md := testMetadata()
	md.Status.Clusters[0].Volumes[0].TemplateName = "cache"

	_, err := BuildWorks(m, md)
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("BuildWorks error = %v, want target/source template mismatch", err)
	}
}

func TestBuildWorksUsesDeterministicTargetIdentityAcrossRequests(t *testing.T) {
	first := testMigration()
	second := testMigration()
	second.Name = "move-web-again"
	second.UID = types.UID("migration-uid-2")
	md := testMetadata()

	firstWorks, err := BuildWorks(first, md)
	if err != nil {
		t.Fatalf("first BuildWorks failed: %v", err)
	}
	secondWorks, err := BuildWorks(second, md)
	if err != nil {
		t.Fatalf("second BuildWorks failed: %v", err)
	}
	if firstWorks[0].GetName() != secondWorks[0].GetName() {
		t.Fatalf("same target claim should produce same Work/PV name across requests, got %q and %q", firstWorks[0].GetName(), secondWorks[0].GetName())
	}

	firstPVName := workManifestName(t, firstWorks[0])
	secondPVName := workManifestName(t, secondWorks[0])
	if firstPVName != secondPVName {
		t.Fatalf("same target claim should produce same PV name across requests, got %q and %q", firstPVName, secondPVName)
	}
}

func TestWorkAppliedRequiresCurrentAppliedEvidenceForManifestPV(t *testing.T) {
	base := appliedWork(t, testMigration(), testMetadata(), "Bound")
	if !workApplied(base.DeepCopy()) {
		t.Fatalf("expected applied Work to be accepted")
	}
	fakeGeneration := base.DeepCopy()
	fakeGeneration.SetGeneration(0)
	conditions, _, _ := unstructured.NestedSlice(fakeGeneration.Object, "status", "conditions")
	conditions[0].(map[string]interface{})["observedGeneration"] = int64(0)
	_ = unstructured.SetNestedSlice(fakeGeneration.Object, conditions, "status", "conditions")
	if !workApplied(fakeGeneration) {
		t.Fatalf("expected generation-zero fake/apiserver-early Work to be accepted with zero observedGeneration")
	}

	tests := []struct {
		name string
		edit func(*unstructured.Unstructured)
	}{
		{
			name: "stale observed generation",
			edit: func(w *unstructured.Unstructured) {
				conditions, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
				conditions[0].(map[string]interface{})["observedGeneration"] = int64(99)
				_ = unstructured.SetNestedSlice(w.Object, conditions, "status", "conditions")
			},
		},
		{
			name: "missing applied condition",
			edit: func(w *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(w.Object, []interface{}{}, "status", "conditions")
			},
		},
		{
			name: "wrong manifest status kind",
			edit: func(w *unstructured.Unstructured) {
				statuses, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
				_ = unstructured.SetNestedField(statuses[0].(map[string]interface{}), "ConfigMap", "identifier", "kind")
				_ = unstructured.SetNestedSlice(w.Object, statuses, "status", "manifestStatuses")
			},
		},
		{
			name: "non-ready PV phase",
			edit: func(w *unstructured.Unstructured) {
				statuses, _, _ := unstructured.NestedSlice(w.Object, "status", "manifestStatuses")
				_ = unstructured.SetNestedField(statuses[0].(map[string]interface{}), "Failed", "status", "phase")
				_ = unstructured.SetNestedSlice(w.Object, statuses, "status", "manifestStatuses")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := base.DeepCopy()
			tt.edit(w)
			if workApplied(w) {
				t.Fatalf("workApplied accepted invalid evidence")
			}
		})
	}
}

func TestMigrationRecordsDurableWorkRefsThenDoesNotRecreateAfterReadyOrCompleted(t *testing.T) {
	ctx := context.Background()
	m := testMigration()
	md := testMetadata()
	sts := testStatefulSet()
	rb := testBinding(true, []string{sourceCluster})
	c := fakeClient(t, m, md, sts, rb)
	r := &PVMigrationReconciler{Client: c, PollInterval: time.Millisecond}
	req := reconcileRequest(m)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("record identities reconcile failed: %v", err)
	}
	after := &api.PVMigration{}
	mustGet(t, c, client.ObjectKeyFromObject(m), after)
	if after.Status.Phase != "Preparing" || len(after.Status.Works) != 1 || after.Status.Works[0].Applied {
		t.Fatalf("unexpected recorded status: %#v", after.Status)
	}
	if got := statusPlanHash(t, after.Status); got == "" {
		t.Fatalf("initial planning reconcile should record a non-empty PlanHash")
	}
	if exists(ctx, c, WorkGVK, after.Status.Works[0].Namespace, after.Status.Works[0].Name) {
		t.Fatalf("first pass should persist identities before creating Work")
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create Work reconcile failed: %v", err)
	}
	if !exists(ctx, c, WorkGVK, after.Status.Works[0].Namespace, after.Status.Works[0].Name) {
		t.Fatalf("second pass should create recorded Work")
	}

	work := appliedWork(t, m, md, "Available")
	work.SetName(after.Status.Works[0].Name)
	work.SetNamespace(after.Status.Works[0].Namespace)
	mustDeleteByKey(t, c, WorkGVK, work.GetNamespace(), work.GetName())
	mustCreate(t, c, work)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("ready reconcile failed: %v", err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), after)
	if after.Status.Phase != "Ready" || !after.Status.Works[0].Applied {
		t.Fatalf("expected Ready with applied durable evidence, got %#v", after.Status)
	}

	mustDelete(t, c, work)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Ready migration should not recreate deleted Work: %v", err)
	}
	if exists(ctx, c, WorkGVK, work.GetNamespace(), work.GetName()) {
		t.Fatalf("Ready migration recreated missing Work")
	}

	after.Status.Phase = "Completed"
	mustStatusUpdate(t, c, after)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Completed migration should be ignored: %v", err)
	}
	if exists(ctx, c, WorkGVK, work.GetNamespace(), work.GetName()) {
		t.Fatalf("Completed migration recreated missing Work")
	}
}

func TestMigrationPlanHashIgnoresCollectionTimeButBlocksSourceMutation(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*api.PVMetadata)
		wantBlock bool
	}{
		{
			name: "collectedAt refresh allowed",
			mutate: func(md *api.PVMetadata) {
				later := metav1.NewTime(md.Status.Clusters[0].CollectedAt.Add(time.Minute))
				md.Status.Clusters[0].CollectedAt = &later
			},
		},
		{
			name: "NFS path mutation blocked",
			mutate: func(md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes[0].PVSpec.NFS.Path = "/exports/other"
			},
			wantBlock: true,
		},
		{
			name: "source PVC UID mutation blocked",
			mutate: func(md *api.PVMetadata) {
				md.Status.Clusters[0].Volumes[0].PVCUID = "pvc-uid-mutated"
				md.Status.Clusters[0].Volumes[0].PVSpec.ClaimRef.UID = types.UID("pvc-uid-mutated")
			},
			wantBlock: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m := testMigration()
			md := testMetadata()
			c := fakeClient(t, m, md, testStatefulSet(), testBinding(true, []string{sourceCluster}))
			r := &PVMigrationReconciler{Client: c, PollInterval: time.Millisecond}

			if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
				t.Fatalf("initial reconcile failed: %v", err)
			}
			planned := &api.PVMigration{}
			mustGet(t, c, client.ObjectKeyFromObject(m), planned)
			if got := statusPlanHash(t, planned.Status); got == "" {
				t.Fatalf("initial planning reconcile should record a non-empty PlanHash")
			}

			currentMD := &api.PVMetadata{}
			mustGet(t, c, client.ObjectKeyFromObject(md), currentMD)
			tt.mutate(currentMD)
			mustStatusUpdate(t, c, currentMD)

			if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
				t.Fatalf("second reconcile should report plan drift through status, got error: %v", err)
			}
			mustGet(t, c, client.ObjectKeyFromObject(m), planned)
			workRef := planned.Status.Works[0]
			workExists := exists(ctx, c, WorkGVK, workRef.Namespace, workRef.Name)
			if tt.wantBlock {
				if planned.Status.Phase != "Pending" || workExists {
					t.Fatalf("plan mutation should leave Pending with no Work creation, status=%#v workExists=%v", planned.Status, workExists)
				}
				return
			}
			if planned.Status.Phase != "Preparing" || !workExists {
				t.Fatalf("collectedAt-only refresh should keep plan valid and create Work, status=%#v workExists=%v", planned.Status, workExists)
			}
		})
	}
}

func TestCleanupDeletesWorkOnlyWithDurableEvidenceAndPreservesRecordedRefs(t *testing.T) {
	ctx := context.Background()
	m := testMigration()
	md := testMetadata()
	work := appliedWork(t, m, md, "Bound")
	m.Status = api.PVMigrationStatus{
		ObservedGeneration: m.Generation,
		Phase:              "Ready",
		Works: []api.WorkReference{{
			Name:      work.GetName(),
			Namespace: work.GetNamespace(),
			Applied:   true,
		}},
	}
	c := fakeClient(t, m, work)
	r := &PVCleanupReconciler{Client: c, PollInterval: time.Millisecond}
	req := reconcileRequest(m)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("cleanup delete reconcile failed: %v", err)
	}
	if exists(ctx, c, WorkGVK, work.GetNamespace(), work.GetName()) {
		t.Fatalf("cleanup should delete only the Karmada Work")
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Ready" || len(m.Status.Works) != 1 || m.Status.Works[0].Detached {
		t.Fatalf("first cleanup pass should retain durable evidence until absence is observed, got %#v", m.Status)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("cleanup absence reconcile failed: %v", err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Completed" || !m.Status.Works[0].Detached || !m.Status.Works[0].Applied {
		t.Fatalf("expected Completed with retained applied/detached evidence, got %#v", m.Status)
	}
}

func TestMigrationRejectsForeignOrMutatedWork(t *testing.T) {
	tests := []struct {
		name string
		edit func(*unstructured.Unstructured)
		want string
	}{
		{
			name: "foreign owner",
			edit: func(w *unstructured.Unstructured) {
				w.SetLabels(map[string]string{ManagedLabel: ManagerName, OwnerLabel: "someone-else"})
			},
			want: "ownership or immutable spec conflict",
		},
		{
			name: "mutated generation",
			edit: func(w *unstructured.Unstructured) {
				w.SetGeneration(2)
			},
			want: "modified",
		},
		{
			name: "mutated spec",
			edit: func(w *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(w.Object, false, "spec", "preserveResourcesOnDeletion")
			},
			want: "ownership or immutable spec conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m := testMigration()
			md := testMetadata()
			sts := testStatefulSet()
			rb := testBinding(true, []string{sourceCluster})
			work := appliedWork(t, m, md, "Bound")
			expectedWorks, err := BuildWorks(m, md)
			if err != nil {
				t.Fatalf("BuildWorks failed: %v", err)
			}
			hash, err := planHash(m, md, expectedWorks)
			if err != nil {
				t.Fatalf("planHash failed: %v", err)
			}
			tt.edit(work)
			m.Status = api.PVMigrationStatus{
				ObservedGeneration: m.Generation,
				Phase:              "Preparing",
				PlanHash:           hash,
				Works:              []api.WorkReference{{Name: work.GetName(), Namespace: work.GetNamespace()}},
			}
			c := fakeClient(t, m, md, sts, rb, work)
			r := &PVMigrationReconciler{Client: c, PollInterval: time.Millisecond}

			_, err = r.Reconcile(ctx, reconcileRequest(m))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Reconcile error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMigrationBindingGatesRequireSuspensionAndTargetPreStage(t *testing.T) {
	tests := []struct {
		name string
		edit func(*api.PVMigration, *api.PVMetadata, *appsv1.StatefulSet)
		rb   *unstructured.Unstructured
		want string
	}{
		{
			name: "dispatching not suspended",
			rb:   testBinding(false, []string{sourceCluster}),
			want: "suspend ResourceBinding dispatching",
		},
		{
			name: "target already selected",
			rb:   testBinding(true, []string{sourceCluster, targetCluster}),
			want: "target is already selected",
		},
		{
			name: "target ordinal missing sibling template claim",
			edit: func(_ *api.PVMigration, _ *api.PVMetadata, sts *appsv1.StatefulSet) {
				sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "cache"}})
			},
			rb:   testBinding(true, []string{sourceCluster}),
			want: "target ordinal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m := testMigration()
			md := testMetadata()
			sts := testStatefulSet()
			if tt.edit != nil {
				tt.edit(m, md, sts)
			}
			c := fakeClient(t, m, md, sts, tt.rb)
			r := &PVMigrationReconciler{Client: c, PollInterval: time.Millisecond}

			if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
				t.Fatalf("Reconcile should record Pending status instead of returning gate error: %v", err)
			}
			mustGet(t, c, client.ObjectKeyFromObject(m), m)
			if m.Status.Phase != "Pending" || !strings.Contains(m.Status.Message, tt.want) {
				t.Fatalf("status = %#v, want Pending message containing %q", m.Status, tt.want)
			}
		})
	}
}

func TestMigrationBlocksExistingRecordedClaimForSameTargetPVC(t *testing.T) {
	ctx := context.Background()
	md := testMetadata()
	other := testMigration()
	other.Name = "existing-move-web"
	other.UID = types.UID("migration-uid-existing")
	otherWorks, err := BuildWorks(other, md)
	if err != nil {
		t.Fatalf("existing BuildWorks failed: %v", err)
	}
	other.Status = api.PVMigrationStatus{
		ObservedGeneration: other.Generation,
		Phase:              "Completed",
		Works: []api.WorkReference{{
			Name:      otherWorks[0].GetName(),
			Namespace: otherWorks[0].GetNamespace(),
			Applied:   true,
			Detached:  true,
		}},
	}

	m := testMigration()
	m.Name = "new-move-web"
	m.UID = types.UID("migration-uid-new")
	c := fakeClient(t, m, other, md, testStatefulSet(), testBinding(true, []string{sourceCluster}))
	r := &PVMigrationReconciler{Client: c, PollInterval: time.Millisecond}

	if _, err := r.Reconcile(ctx, reconcileRequest(m)); err != nil {
		t.Fatalf("Reconcile should record Pending status instead of returning target-claim conflict: %v", err)
	}
	mustGet(t, c, client.ObjectKeyFromObject(m), m)
	if m.Status.Phase != "Pending" || !strings.Contains(m.Status.Message, "target PVC") {
		t.Fatalf("status = %#v, want Pending target PVC conflict", m.Status)
	}
}

func TestOverlappingPendingRequestsHaveOneDeterministicWinner(t *testing.T) {
	ctx := context.Background()
	first, second := testMigration(), testMigration()
	first.Name, first.UID = "first", types.UID("a")
	second.Name, second.UID = "second", types.UID("b")
	md := testMetadata()
	c := fakeClient(t, first, second, md, testStatefulSet(), testBinding(true, []string{sourceCluster}))
	r := &PVMigrationReconciler{Client: c}
	if err := r.validateBinding(ctx, first, md); err != nil {
		t.Fatalf("first should win: %v", err)
	}
	if err := r.validateBinding(ctx, second, md); err == nil {
		t.Fatal("second request must wait for reserved target PVC")
	}
}

func TestDiscoveryCreatesStickySourceMetadataAndPolicyWithoutMemberAPIs(t *testing.T) {
	ctx := context.Background()
	sts := testStatefulSet()
	rb := testBinding(true, []string{sourceCluster})
	c := fakeClient(t, sts, rb)
	r := &MetadataDiscovery{Client: c, PollInterval: time.Millisecond}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rb)}); err != nil {
		t.Fatalf("discovery reconcile failed: %v", err)
	}

	mdName := stableName("pvm", string(sts.UID), sourceCluster)
	md := &api.PVMetadata{}
	mustGet(t, c, types.NamespacedName{Namespace: testNamespace, Name: mdName}, md)
	if md.Spec.SourceCluster != sourceCluster || md.Spec.WorkloadRef.Name != stsName || md.Spec.WorkloadRef.UID != string(sts.UID) {
		t.Fatalf("unexpected PVMetadata spec: %#v", md.Spec)
	}

	pp := object(PolicyGVK, testNamespace, mdName)
	mustGet(t, c, client.ObjectKeyFromObject(pp), pp)
	names, _, _ := unstructured.NestedStringSlice(pp.Object, "spec", "placement", "clusterAffinity", "clusterNames")
	if len(names) != 1 || names[0] != sourceCluster {
		t.Fatalf("PropagationPolicy should stay pinned to source cluster, got %#v", names)
	}

	_ = unstructured.SetNestedStringSlice(pp.Object, []string{targetCluster}, "spec", "placement", "clusterAffinity", "clusterNames")
	mustUpdate(t, c, pp)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rb)})
	if err == nil || !strings.Contains(err.Error(), "retargeted") {
		t.Fatalf("expected sticky source retarget error, got %v", err)
	}
}

func TestAPISchemeRegistersExpectedMetaNames(t *testing.T) {
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	assertKinds := map[schema.GroupVersionKind]client.Object{
		{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMetadata"}:  &api.PVMetadata{},
		{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"}: &api.PVMigration{},
	}
	for wantGVK, obj := range assertKinds {
		gvks, _, err := s.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("ObjectKinds(%T) failed: %v", obj, err)
		}
		if len(gvks) != 1 || gvks[0] != wantGVK {
			t.Fatalf("ObjectKinds(%T) = %#v, want %#v", obj, gvks, wantGVK)
		}
	}
}

func testMigration() *api.PVMigration {
	return &api.PVMigration{
		TypeMeta: metav1.TypeMeta{APIVersion: "migration.dcnlab.com/v1alpha1", Kind: "PVMigration"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "move-web",
			Namespace:  testNamespace,
			UID:        types.UID(migrationUID),
			Generation: 1,
		},
		Spec: api.PVMigrationSpec{
			MetadataRef:     "web-member-a",
			SourceCluster:   sourceCluster,
			TargetCluster:   targetCluster,
			ResourceBinding: "web-binding",
			SourceFenced:    true,
			Volumes:         []api.VolumeMapping{{SourcePVC: pvcName, TargetPVC: targetPVCName}},
		},
	}
}

func testMetadata() *api.PVMetadata {
	now := metav1.Now()
	return &api.PVMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "migration.dcnlab.com/v1alpha1", Kind: "PVMetadata"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "web-member-a",
			Namespace:  testNamespace,
			UID:        "metadata-uid-1",
			Generation: 1,
			Labels:     map[string]string{ManagedLabel: ManagerName},
		},
		Spec: api.PVMetadataSpec{
			SourceCluster: sourceCluster,
			WorkloadRef: api.WorkloadRef{
				Name: stsName,
				UID:  stsUID,
			},
		},
		Status: api.PVMetadataStatus{
			Clusters: []api.ClusterSnapshot{{
				ClusterName:        sourceCluster,
				ObservedGeneration: 1,
				Ready:              true,
				CollectedAt:        &now,
				WorkloadUID:        stsUID,
				Volumes: []api.VolumeMetadata{{
					PVCName:      pvcName,
					PVCUID:       pvcUID,
					TemplateName: "data",
					Ordinal:      0,
					PVName:       "pv-data-web-0",
					PVSpec:       portableNFSSpec(),
				}},
			}},
		},
	}
}

func portableNFSSpec() corev1.PersistentVolumeSpec {
	return corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("10Gi"),
		},
		AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
		PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "10.0.0.7", Path: "/exports/web"},
		},
		ClaimRef: &corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  testNamespace,
			Name:       pvcName,
			UID:        types.UID(pvcUID),
		},
	}
}

func testStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: testNamespace,
			UID:       types.UID(stsUID),
			Labels:    map[string]string{OptInLabel: "enabled"},
		},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
			}},
		},
	}
}

func testBinding(suspended bool, clusters []string) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(clusters))
	for _, name := range clusters {
		items = append(items, map[string]interface{}{"name": name})
	}
	rb := object(BindingGVK, testNamespace, "web-binding")
	rb.Object["spec"] = map[string]interface{}{
		"resource": map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "StatefulSet",
			"name":       stsName,
		},
		"suspension": map[string]interface{}{"dispatching": suspended},
		"clusters":   items,
	}
	return rb
}

func appliedWork(t *testing.T, m *api.PVMigration, md *api.PVMetadata, phase string) *unstructured.Unstructured {
	t.Helper()
	works, err := BuildWorks(m, md)
	if err != nil {
		t.Fatalf("BuildWorks failed: %v", err)
	}
	w := works[0]
	w.SetGeneration(1)
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	name, _, _ := unstructured.NestedString(manifests[0].(map[string]interface{}), "metadata", "name")
	w.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{map[string]interface{}{
			"type":               "Applied",
			"status":             "True",
			"observedGeneration": int64(1),
		}},
		"manifestStatuses": []interface{}{map[string]interface{}{
			"identifier": map[string]interface{}{
				"group":   "",
				"version": "v1",
				"kind":    "PersistentVolume",
				"name":    name,
			},
			"status": map[string]interface{}{"phase": phase},
		}},
	}
	return w
}

func workManifestName(t *testing.T, w *unstructured.Unstructured) string {
	t.Helper()
	manifests, _, _ := unstructured.NestedSlice(w.Object, "spec", "workload", "manifests")
	if len(manifests) != 1 {
		t.Fatalf("expected one manifest, got %d", len(manifests))
	}
	manifest, ok := manifests[0].(map[string]interface{})
	if !ok {
		t.Fatalf("manifest is not an object: %#v", manifests[0])
	}
	name, _, _ := unstructured.NestedString(manifest, "metadata", "name")
	return name
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("register api scheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("register apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("register core scheme: %v", err)
	}
	registerUnstructured(t, s, BindingGVK)
	registerUnstructured(t, s, WorkGVK)
	registerUnstructured(t, s, PolicyGVK)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&api.PVMigration{}, &api.PVMetadata{}).
		Build()
}

func registerUnstructured(t *testing.T, s *runtime.Scheme, gvk schema.GroupVersionKind) {
	t.Helper()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
}

func reconcileRequest(obj client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

func mustGet(t *testing.T, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatalf("Get(%s) failed: %v", key.String(), err)
	}
}

func mustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create(%T/%s) failed: %v", obj, obj.GetName(), err)
	}
}

func mustUpdate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatalf("Update(%T/%s) failed: %v", obj, obj.GetName(), err)
	}
}

func mustStatusUpdate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Status().Update(context.Background(), obj); err != nil {
		t.Fatalf("Status().Update(%T/%s) failed: %v", obj, obj.GetName(), err)
	}
}

func statusPlanHash(t *testing.T, status api.PVMigrationStatus) string {
	t.Helper()
	v := reflect.ValueOf(status)
	field := v.FieldByName("PlanHash")
	if !field.IsValid() {
		t.Fatalf("PVMigrationStatus is missing PlanHash")
	}
	if field.Kind() != reflect.String {
		t.Fatalf("PVMigrationStatus.PlanHash kind = %s, want string", field.Kind())
	}
	return field.String()
}

func mustDelete(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Delete(context.Background(), obj); err != nil {
		t.Fatalf("Delete(%T/%s) failed: %v", obj, obj.GetName(), err)
	}
}

func mustDeleteByKey(t *testing.T, c client.Client, gvk schema.GroupVersionKind, namespace, name string) {
	t.Helper()
	obj := object(gvk, namespace, name)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("Get before delete failed: %v", err)
	}
	mustDelete(t, c, obj)
}

func exists(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) bool {
	obj := object(gvk, namespace, name)
	return c.Get(ctx, client.ObjectKeyFromObject(obj), obj) == nil
}
