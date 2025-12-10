package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	snapshotfake "github.com/kubernetes-csi/external-snapshotter/client/v6/clientset/versioned/fake"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func createAdmissionRequest(pvc corev1.PersistentVolumeClaim) *admissionv1.AdmissionRequest {
	rawPVC, _ := json.Marshal(pvc)
	return &admissionv1.AdmissionRequest{
		UID:       "test-uid",
		Namespace: pvc.Namespace,
		Name:      pvc.Name,
		Kind: metav1.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "PersistentVolumeClaim",
		},
		Object: runtime.RawExtension{Raw: rawPVC},
	}
}

func createPVCFromSnapshot(namespace, pvcName, snapshotName, requestedSize string) corev1.PersistentVolumeClaim {
	apiGroup := "snapshot.storage.k8s.io"
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"cdi.kubevirt.io/applyStorageProfile": "true",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     snapshotName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(requestedSize),
				},
			},
		},
	}
}

func createPVCFromSnapshotRef(pvcNamespace, pvcName, snapshotNamespace, snapshotName, requestedSize string) corev1.PersistentVolumeClaim {
	apiGroup := "snapshot.storage.k8s.io"
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: pvcNamespace,
			Labels: map[string]string{
				"cdi.kubevirt.io/applyStorageProfile": "true",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			DataSourceRef: &corev1.TypedObjectReference{
				APIGroup:  &apiGroup,
				Kind:      "VolumeSnapshot",
				Name:      snapshotName,
				Namespace: &snapshotNamespace,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(requestedSize),
				},
			},
		},
	}
}

func createSnapshot(namespace, name, restoreSize string) *snapshotv1.VolumeSnapshot {
	qty := resource.MustParse(restoreSize)
	return &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			RestoreSize: &qty,
		},
	}
}

func TestOversizedPVCGetsShrunk_DataSource(t *testing.T) {
	snapshot := createSnapshot("test-ns", "my-snapshot", "20Gi")
	client := snapshotfake.NewSimpleClientset(snapshot)

	pvc := createPVCFromSnapshot("test-ns", "my-pvc", "my-snapshot", "21Gi")
	req := createAdmissionRequest(pvc)

	resp := reviewPVC(context.Background(), req, client)

	if !resp.Allowed {
		t.Fatal("expected PVC to be allowed")
	}
	if resp.Patch == nil {
		t.Fatal("expected a patch to shrink the PVC")
	}

	var patches []patch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	if len(patches) != 1 || patches[0].Value != "20Gi" {
		t.Fatalf("expected patch to shrink to 20Gi, got %v", patches)
	}
}

func TestOversizedPVCGetsShrunk_DataSourceRef(t *testing.T) {
	snapshot := createSnapshot("snapshot-ns", "my-snapshot", "20Gi")
	client := snapshotfake.NewSimpleClientset(snapshot)

	pvc := createPVCFromSnapshotRef("pvc-ns", "my-pvc", "snapshot-ns", "my-snapshot", "21Gi")
	req := createAdmissionRequest(pvc)

	resp := reviewPVC(context.Background(), req, client)

	if !resp.Allowed {
		t.Fatal("expected PVC to be allowed")
	}
	if resp.Patch == nil {
		t.Fatal("expected a patch to shrink the PVC")
	}

	var patches []patch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	if len(patches) != 1 || patches[0].Value != "20Gi" {
		t.Fatalf("expected patch to shrink to 20Gi, got %v", patches)
	}
}

func TestCorrectlySizedPVCIsUntouched(t *testing.T) {
	snapshot := createSnapshot("test-ns", "my-snapshot", "20Gi")
	client := snapshotfake.NewSimpleClientset(snapshot)

	pvc := createPVCFromSnapshot("test-ns", "my-pvc", "my-snapshot", "20Gi")
	req := createAdmissionRequest(pvc)

	resp := reviewPVC(context.Background(), req, client)

	if !resp.Allowed {
		t.Fatal("expected PVC to be allowed")
	}
	if resp.Patch != nil {
		t.Fatal("expected no patch when PVC size matches RestoreSize")
	}
}

func TestHTTPEndpoint(t *testing.T) {
	snapshot := createSnapshot("test-ns", "my-snapshot", "20Gi")
	client := snapshotfake.NewSimpleClientset(snapshot)

	pvc := createPVCFromSnapshot("test-ns", "my-pvc", "my-snapshot", "21Gi")
	admissionReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: createAdmissionRequest(pvc),
	}

	body, _ := json.Marshal(admissionReview)
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleMutate(w, req, client)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var response admissionv1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if !response.Response.Allowed || response.Response.Patch == nil {
		t.Fatal("expected allowed response with patch")
	}
}
