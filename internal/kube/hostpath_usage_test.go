package kube

import (
	"context"
	"testing"

	"github.com/labring-sigs/pvc-migrate/internal/domain"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientfake "k8s.io/client-go/kubernetes/fake"
)

func TestHostPathUsagePodIsReadOnlyAndConstrained(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data", UID: types.UID("pv-uid")},
		Spec: corev1.PersistentVolumeSpec{
			HostPath: &corev1.HostPathVolumeSource{Path: "/data/openebs"},
			ClaimRef: &corev1.ObjectReference{Namespace: "app", Name: "data", UID: types.UID("pvc-uid")},
		},
	}
	pod := hostPathUsagePod("example/tool:test", "usage", "app", "data", "session", pv)
	if pod.Spec.Containers[0].VolumeMounts[0].ReadOnly != true ||
		pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly != true {
		t.Fatal("usage Pod must mount source PVC read-only")
	}
	if pod.Labels[SessionKey] != "session" {
		t.Fatalf("usage Pod session label=%q", pod.Labels[SessionKey])
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("usage Pod must not mount a service account token")
	}
	if pod.Spec.Containers[0].SecurityContext == nil ||
		pod.Spec.Containers[0].SecurityContext.RunAsUser == nil ||
		*pod.Spec.Containers[0].SecurityContext.RunAsUser != 0 {
		t.Fatal("usage Pod must run as root for filesystem metadata access")
	}
}

func TestHostPathUsageReaderRejectsIdentityMismatchBeforeCreatingPod(t *testing.T) {
	client := clientfake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app", UID: types.UID("actual")},
	})
	reader := NewHostPathUsageReader(client, "example/tool:test")
	_, err := reader.Read(context.Background(), VolumeUsageReadOptions{
		SourcePVC: domain.ObjectReference{Namespace: "app", Name: "data", UID: types.UID("expected")},
		SourcePV:  domain.ObjectReference{Name: "pv-data", UID: types.UID("pv-uid")},
	})
	if err == nil {
		t.Fatal("identity mismatch unexpectedly measured")
	}
	if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
		t.Fatalf("unexpected actions=%v", client.Actions())
	}
}

func TestIsOpenEBSHostPathStorageClass(t *testing.T) {
	for _, test := range []struct {
		name string
		sc   *storagev1.StorageClass
		want bool
	}{
		{name: "parameter", sc: &storagev1.StorageClass{Provisioner: OpenEBSLocalPVProvisioner, Parameters: map[string]string{"storageType": "hostpath"}}, want: true},
		{name: "annotation", sc: &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"cas.openebs.io/config": "- name: StorageType\n  value: hostpath"}}, Provisioner: OpenEBSLocalPVProvisioner}, want: true},
		{name: "other", sc: &storagev1.StorageClass{Provisioner: "example.io", Parameters: map[string]string{"storageType": "hostpath"}}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsOpenEBSHostPathStorageClass(test.sc); got != test.want {
				t.Fatalf("got=%t want=%t", got, test.want)
			}
		})
	}
}
