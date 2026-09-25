package member

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
)

func TestPVSyncCollectsBoundVolumes(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	c := fakeClient(t, metadata, statefulSet("web", "ns", "local-sts", 2, 0, "data"), pvc("data-web-0", "ns", "pv-0", "pvc-0", corev1.ClaimBound), pvc("data-web-1", "ns", "pv-1", "pvc-1", corev1.ClaimBound), pv("pv-0", "ns", "data-web-0", "pvc-0", corev1.VolumeBound), pv("pv-1", "ns", "data-web-1", "pvc-1", corev1.VolumeBound))

	result, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %v, want 1s", result.RequeueAfter)
	}

	got := getMetadata(t, ctx, c, metadata)
	if !got.Status.Ready {
		t.Fatalf("Ready = false, message %q", got.Status.Message)
	}
	if got.Status.WorkloadUID != "local-sts" {
		t.Fatalf("WorkloadUID = %q, want local-sts", got.Status.WorkloadUID)
	}
	if len(got.Status.Volumes) != 2 {
		t.Fatalf("volumes = %d, want 2", len(got.Status.Volumes))
	}
	if got.Status.Volumes[0].PVCName != "data-web-0" || got.Status.Volumes[0].PVName != "pv-0" {
		t.Fatalf("unexpected first volume: %#v", got.Status.Volumes[0])
	}
	if got.Status.Volumes[0].PVSpec.ClaimRef == nil || got.Status.Volumes[0].PVSpec.ClaimRef.UID != types.UID("pvc-0") {
		t.Fatalf("PVSpec ClaimRef UID not preserved: %#v", got.Status.Volumes[0].PVSpec.ClaimRef)
	}
	if got.Status.CollectedAt == nil {
		t.Fatal("CollectedAt is nil on successful collection")
	}
	if len(got.Status.Clusters) != 0 {
		t.Fatalf("member wrote management-owned clusters: %#v", got.Status.Clusters)
	}
}

func TestPVSyncPreservesLastGoodSnapshotOnMissingPVC(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	lastCollected := metav1.NewTime(time.Date(2026, 9, 25, 1, 2, 3, 0, time.UTC))
	metadata.Status = migrationv1alpha1.PVMetadataStatus{
		ObservedGeneration: 6,
		Ready:              true,
		Message:            "collected",
		CollectedAt:        &lastCollected,
		WorkloadUID:        "old-local",
		Volumes: []migrationv1alpha1.VolumeMetadata{{
			PVCName: "data-web-0",
			PVCUID:  "old-pvc",
			PVName:  "old-pv",
		}},
	}
	c := fakeClient(t, metadata, statefulSet("web", "ns", "new-local", 1, 0, "data"))

	_, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getMetadata(t, ctx, c, metadata)
	if got.Status.Ready {
		t.Fatal("Ready = true, want false")
	}
	if !strings.Contains(got.Status.Message, "pvc ns/data-web-0 not found") {
		t.Fatalf("Message = %q, want missing pvc", got.Status.Message)
	}
	if got.Status.ObservedGeneration != 6 {
		t.Fatalf("ObservedGeneration = %d, want last good 6", got.Status.ObservedGeneration)
	}
	if got.Status.CollectedAt == nil || !got.Status.CollectedAt.Equal(&lastCollected) {
		t.Fatalf("CollectedAt = %v, want preserved %v", got.Status.CollectedAt, &lastCollected)
	}
	if len(got.Status.Volumes) != 1 || got.Status.Volumes[0].PVName != "old-pv" {
		t.Fatalf("volumes were not preserved: %#v", got.Status.Volumes)
	}
	if got.Status.WorkloadUID != "old-local" {
		t.Fatalf("WorkloadUID = %q, want last good old-local", got.Status.WorkloadUID)
	}
	if len(got.Status.Clusters) != 0 {
		t.Fatalf("member wrote management-owned clusters: %#v", got.Status.Clusters)
	}
}

func TestPVSyncRejectsPVWithWrongClaim(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	c := fakeClient(t, metadata, statefulSet("web", "ns", "local-sts", 1, 0, "data"), pvc("data-web-0", "ns", "pv-0", "pvc-0", corev1.ClaimBound), pv("pv-0", "ns", "other", "pvc-0", corev1.VolumeBound))

	_, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getMetadata(t, ctx, c, metadata)
	if got.Status.Ready {
		t.Fatal("Ready = true, want false")
	}
	if !strings.Contains(got.Status.Message, "claimRef does not match") {
		t.Fatalf("Message = %q, want wrong claim message", got.Status.Message)
	}
	if len(got.Status.Volumes) != 0 {
		t.Fatalf("volumes = %#v, want none without last good snapshot", got.Status.Volumes)
	}
}

func TestPVSyncRejectsUnboundPVCAndPV(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	c := fakeClient(t, metadata, statefulSet("web", "ns", "local-sts", 1, 0, "data"), pvc("data-web-0", "ns", "pv-0", "pvc-0", corev1.ClaimPending), pv("pv-0", "ns", "data-web-0", "pvc-0", corev1.VolumeBound))

	_, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getMetadata(t, ctx, c, metadata)
	if !strings.Contains(got.Status.Message, "pvc ns/data-web-0 is not bound") {
		t.Fatalf("Message = %q, want unbound pvc", got.Status.Message)
	}

	metadata = pvMetadata("meta-pv", "ns", "web", "member-a")
	c = fakeClient(t, metadata, statefulSet("web", "ns", "local-sts", 1, 0, "data"), pvc("data-web-0", "ns", "pv-0", "pvc-0", corev1.ClaimBound), pv("pv-0", "ns", "data-web-0", "pvc-0", corev1.VolumePending))
	_, err = reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got = getMetadata(t, ctx, c, metadata)
	if !strings.Contains(got.Status.Message, "pv pv-0 is not bound") {
		t.Fatalf("Message = %q, want unbound pv", got.Status.Message)
	}
}

func TestPVSyncIgnoresForeignCluster(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "other")
	c := fakeClient(t, metadata)

	result, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want no requeue", result.RequeueAfter)
	}

	got := getMetadata(t, ctx, c, metadata)
	if got.Status.ObservedGeneration != 0 || got.Status.Ready {
		t.Fatalf("foreign cluster status changed: %#v", got.Status)
	}
}

func TestPVSyncCollectsMultipleTemplatesWithOrdinalStart(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	c := fakeClient(t,
		metadata,
		statefulSet("web", "ns", "local-sts", 2, 3, "data", "logs"),
		pvc("data-web-3", "ns", "pv-data-3", "pvc-data-3", corev1.ClaimBound),
		pvc("data-web-4", "ns", "pv-data-4", "pvc-data-4", corev1.ClaimBound),
		pvc("logs-web-3", "ns", "pv-logs-3", "pvc-logs-3", corev1.ClaimBound),
		pvc("logs-web-4", "ns", "pv-logs-4", "pvc-logs-4", corev1.ClaimBound),
		pv("pv-data-3", "ns", "data-web-3", "pvc-data-3", corev1.VolumeBound),
		pv("pv-data-4", "ns", "data-web-4", "pvc-data-4", corev1.VolumeBound),
		pv("pv-logs-3", "ns", "logs-web-3", "pvc-logs-3", corev1.VolumeBound),
		pv("pv-logs-4", "ns", "logs-web-4", "pvc-logs-4", corev1.VolumeBound),
	)

	_, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getMetadata(t, ctx, c, metadata)
	if len(got.Status.Volumes) != 4 {
		t.Fatalf("volumes = %d, want 4", len(got.Status.Volumes))
	}
	want := []struct {
		pvc      string
		template string
		ordinal  int32
	}{
		{"data-web-3", "data", 3},
		{"data-web-4", "data", 4},
		{"logs-web-3", "logs", 3},
		{"logs-web-4", "logs", 4},
	}
	for i, volume := range got.Status.Volumes {
		if volume.PVCName != want[i].pvc || volume.TemplateName != want[i].template || volume.Ordinal != want[i].ordinal {
			t.Fatalf("volume[%d] = %#v, want %#v", i, volume, want[i])
		}
	}
}

func TestPVSyncZeroReplicasIsNotReady(t *testing.T) {
	ctx := context.Background()
	metadata := pvMetadata("meta", "ns", "web", "member-a")
	c := fakeClient(t, metadata, statefulSet("web", "ns", "local-sts", 0, 0, "data"))

	_, err := reconciler(c).Reconcile(ctx, request(metadata))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getMetadata(t, ctx, c, metadata)
	if got.Status.Ready {
		t.Fatal("Ready = true, want false")
	}
	if !strings.Contains(got.Status.Message, "zero replicas") {
		t.Fatalf("Message = %q, want zero replicas", got.Status.Message)
	}
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&migrationv1alpha1.PVMetadata{}).
		WithObjects(objs...).
		Build()
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("apps AddToScheme: %v", err)
	}
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("migration AddToScheme: %v", err)
	}
	return scheme
}

func reconciler(c client.Client) *PVSyncReconciler {
	return &PVSyncReconciler{Client: c, ClusterName: "member-a", PollInterval: time.Second}
}

func request(metadata *migrationv1alpha1.PVMetadata) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: metadata.Namespace, Name: metadata.Name}}
}

func getMetadata(t *testing.T, ctx context.Context, c client.Client, metadata *migrationv1alpha1.PVMetadata) migrationv1alpha1.PVMetadata {
	t.Helper()
	var got migrationv1alpha1.PVMetadata
	if err := c.Get(ctx, types.NamespacedName{Namespace: metadata.Namespace, Name: metadata.Name}, &got); err != nil {
		t.Fatalf("Get metadata: %v", err)
	}
	return got
}

func pvMetadata(name, namespace, workload, sourceCluster string) *migrationv1alpha1.PVMetadata {
	return &migrationv1alpha1.PVMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: migrationv1alpha1.GroupVersion.String(),
			Kind:       "PVMetadata",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 7,
		},
		Spec: migrationv1alpha1.PVMetadataSpec{
			WorkloadRef:   migrationv1alpha1.WorkloadRef{Name: workload, UID: "management-template"},
			SourceCluster: sourceCluster,
		},
	}
}

func statefulSet(name, namespace, uid string, replicas, start int32, templates ...string) *appsv1.StatefulSet {
	claims := make([]corev1.PersistentVolumeClaim, 0, len(templates))
	for _, template := range templates {
		claims = append(claims, corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: template},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid)},
		Spec: appsv1.StatefulSetSpec{
			Replicas:             &replicas,
			ServiceName:          "svc",
			Ordinals:             &appsv1.StatefulSetOrdinals{Start: start},
			VolumeClaimTemplates: claims,
		},
	}
}

func pvc(name, namespace, pvName, uid string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid)},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: pvName,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func pv(name, namespace, claimName, claimUID string, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			ClaimRef: &corev1.ObjectReference{
				Namespace: namespace,
				Name:      claimName,
				UID:       types.UID(claimUID),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/test"},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
}
