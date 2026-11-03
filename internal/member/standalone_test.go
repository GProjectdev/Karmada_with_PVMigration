package member

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPVSyncStandaloneClaims(t *testing.T) {
	for _, withVCT := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-only", true: "mixed-shadow-dedup"}[withVCT], func(t *testing.T) {
			md := pvMetadata("meta", "ns", "web", "member-a")
			sts := statefulSet("web", "ns", "member-sts", 2, 0)
			sts.Spec.Template.Annotations = map[string]string{"fluidcr.dcnlab.com/checkpoint-claim": "checkpoint"}
			sts.Spec.Template.Spec.Volumes = []corev1.Volume{
				{Name: "dataset", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "dataset"}}},
				{Name: "duplicate", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "dataset"}}},
				{Name: "checkpoint", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "checkpoint"}}},
			}
			objects := []client.Object{md, sts}
			if withVCT {
				sts.Spec.VolumeClaimTemplates = statefulSet("web", "ns", "member-sts", 2, 0, "data").Spec.VolumeClaimTemplates
				sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes,
					corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shadowed-missing"}}},
					corev1.Volume{Name: "ordinal-alias", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-web-0"}}})
				for _, name := range []string{"data-web-0", "data-web-1"} {
					objects = append(objects, pvc(name, "ns", "pv-"+name, "uid-"+name, corev1.ClaimBound), pv("pv-"+name, "ns", name, "uid-"+name, corev1.VolumeBound))
				}
			}
			for _, name := range []string{"checkpoint", "dataset"} {
				objects = append(objects, pvc(name, "ns", "pv-"+name, "uid-"+name, corev1.ClaimBound), pv("pv-"+name, "ns", name, "uid-"+name, corev1.VolumeBound))
			}
			c := fakeClient(t, objects...)
			if _, err := reconciler(c).Reconcile(context.Background(), request(md)); err != nil {
				t.Fatal(err)
			}
			got := getMetadata(t, context.Background(), c, md)
			want := 2
			if withVCT {
				want = 4
			}
			if !got.Status.Ready || len(got.Status.Volumes) != want {
				t.Fatalf("unexpected collection: %#v", got.Status)
			}
			for _, v := range got.Status.Volumes {
				shared := v.PVCName == "checkpoint" || v.PVCName == "dataset"
				if v.Standalone != shared || (v.PVCSpec != nil) != shared {
					t.Fatalf("wrong marker: %#v", v)
				}
				if shared && (v.TemplateName != "" || v.PVCSpec.VolumeName != v.PVName) {
					t.Fatalf("wrong spec: %#v", v)
				}
			}
		})
	}
}

func TestPVSyncAnnotationOnlyClaimMustExist(t *testing.T) {
	md := pvMetadata("meta", "ns", "web", "member-a")
	sts := statefulSet("web", "ns", "member-sts", 1, 0)
	sts.Spec.Template.Annotations = map[string]string{"fluidcr.dcnlab.com/checkpoint-claim": "checkpoint"}
	c := fakeClient(t, md, sts)
	if _, err := reconciler(c).Reconcile(context.Background(), request(md)); err != nil {
		t.Fatal(err)
	}
	if got := getMetadata(t, context.Background(), c, md); got.Status.Ready {
		t.Fatal("missing injected claim accepted")
	}
	for _, obj := range []client.Object{pvc("checkpoint", "ns", "pv-checkpoint", "uid-checkpoint", corev1.ClaimBound), pv("pv-checkpoint", "ns", "checkpoint", "uid-checkpoint", corev1.VolumeBound)} {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reconciler(c).Reconcile(context.Background(), request(md)); err != nil {
		t.Fatal(err)
	}
	if got := getMetadata(t, context.Background(), c, md); !got.Status.Ready || len(got.Status.Volumes) != 1 || !got.Status.Volumes[0].Standalone {
		t.Fatalf("annotation not collected: %#v", got.Status)
	}
}
