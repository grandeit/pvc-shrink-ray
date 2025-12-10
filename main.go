package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	snapshotclient "github.com/kubernetes-csi/external-snapshotter/client/v6/clientset/versioned"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type patch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func reviewPVC(ctx context.Context, req *admissionv1.AdmissionRequest, client snapshotclient.Interface) *admissionv1.AdmissionResponse {
	var pvc corev1.PersistentVolumeClaim

	if err := json.Unmarshal(req.Object.Raw, &pvc); err != nil {
		klog.Errorf("Could not unmarshal PVC: %v", err)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	klog.Infof("Reviewing PVC: %s/%s (UID: %s)", pvc.Namespace, pvc.Name, string(req.UID))

	if pvc.Labels == nil || pvc.Labels["cdi.kubevirt.io/applyStorageProfile"] != "true" {
		klog.Warningf("PVC %s/%s doesn't have cdi.kubevirt.io/applyStorageProfile=true label - This should not happen, skipping", pvc.Namespace, pvc.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	snapshotName, snapshotNamespace := "", ""
	if pvc.Spec.DataSource != nil {
		if pvc.Spec.DataSource.Kind == "VolumeSnapshot" &&
			pvc.Spec.DataSource.APIGroup != nil && *pvc.Spec.DataSource.APIGroup == snapshotv1.GroupName {
			snapshotName = pvc.Spec.DataSource.Name
			snapshotNamespace = pvc.Namespace
		}
	}
	if snapshotName == "" && pvc.Spec.DataSourceRef != nil {
		if pvc.Spec.DataSourceRef.Kind == "VolumeSnapshot" &&
			pvc.Spec.DataSourceRef.APIGroup != nil && *pvc.Spec.DataSourceRef.APIGroup == snapshotv1.GroupName {
			snapshotName = pvc.Spec.DataSourceRef.Name
			snapshotNamespace = pvc.Namespace
			if pvc.Spec.DataSourceRef.Namespace != nil && *pvc.Spec.DataSourceRef.Namespace != "" {
				snapshotNamespace = *pvc.Spec.DataSourceRef.Namespace
			}
		}
	}

	if snapshotName == "" {
		klog.Infof("PVC %s/%s doesn't have a VolumeSnapshot as DataSource, skipping", pvc.Namespace, pvc.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	snapshot, err := client.SnapshotV1().VolumeSnapshots(snapshotNamespace).Get(ctx, snapshotName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Could not get VolumeSnapshot %s/%s: %v", snapshotNamespace, snapshotName, err)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	var snapshotRestoreSize *resource.Quantity
	if snapshot.Status != nil && snapshot.Status.RestoreSize != nil {
		snapshotRestoreSize = snapshot.Status.RestoreSize
	}

	if snapshotRestoreSize == nil {
		klog.Warningf("VolumeSnapshot %s/%s of PVC %s/%s has no RestoreSize, skipping", snapshotNamespace, snapshotName, pvc.Namespace, pvc.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	pvcSize, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		klog.Warningf("PVC %s/%s has no storage request, skipping", pvc.Namespace, pvc.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	if pvcSize.Cmp(*snapshotRestoreSize) > 0 {
		klog.Infof("Oversized PVC detected %s/%s, charging the shrink ray...", pvc.Namespace, pvc.Name)

		p := []patch{
			{
				Op:    "replace",
				Path:  "/spec/resources/requests/storage",
				Value: snapshotRestoreSize.String(),
			},
		}

		patchBytes, err := json.Marshal(p)
		if err != nil {
			klog.Errorf("Could not marshal patch: %v", err)
			return &admissionv1.AdmissionResponse{
				Allowed: true,
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}

		klog.Infof("Zapping oversized PVC %s/%s! Reducing size from %s to RestoreSize %s (VolumeSnapshot: %s/%s)",
			pvc.Namespace, pvc.Name, pvcSize.String(), snapshotRestoreSize.String(), snapshotNamespace, snapshotName)

		return &admissionv1.AdmissionResponse{
			Allowed: true,
			Patch:   patchBytes,
			PatchType: func() *admissionv1.PatchType {
				pt := admissionv1.PatchTypeJSONPatch
				return &pt
			}(),
		}
	}

	klog.Infof("PVC %s/%s size %s <= RestoreSize %s is already smol, skipping", pvc.Namespace, pvc.Name, pvcSize.String(), snapshotRestoreSize.String())
	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

func handleMutate(w http.ResponseWriter, r *http.Request, client snapshotclient.Interface) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		klog.Errorf("Invalid request method: %s", r.Method)
		http.Error(w, "invalid request method", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		klog.Errorf("Invalid content type: %s", r.Header.Get("Content-Type"))
		http.Error(w, "invalid content type, expected application/json", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		klog.Errorf("Could not read request body: %v", err)
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	if len(body) == 0 {
		klog.Error("Empty request body")
		http.Error(w, "empty request body", http.StatusBadRequest)
		return
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, fmt.Sprintf("failed to unmarshal admission review: %v", err), http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil {
		klog.Error("Admission review request is missing")
		http.Error(w, "admission review request is missing", http.StatusBadRequest)
		return
	}

	if admissionReview.Request.Kind.Group != "" || admissionReview.Request.Kind.Kind != "PersistentVolumeClaim" {
		klog.Warningf("Unsupported GVK %s - This should not happen, skipping.", admissionReview.Request.Kind.String())
		admissionReview.Response = &admissionv1.AdmissionResponse{
			Allowed: true,
		}
		if err := writeAdmissionReviewResponse(w, &admissionReview); err != nil {
			klog.Errorf("Could not write response: %v", err)
		}
		return
	}

	admissionReview.Response = reviewPVC(r.Context(), admissionReview.Request, client)
	admissionReview.Response.UID = admissionReview.Request.UID

	if err := writeAdmissionReviewResponse(w, &admissionReview); err != nil {
		klog.Errorf("Could not write response: %v", err)
	}
}

func writeAdmissionReviewResponse(w http.ResponseWriter, review *admissionv1.AdmissionReview) error {
	resp, err := json.Marshal(review)
	if err != nil {
		klog.Errorf("Could not marshal response: %v", err)
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp); err != nil {
		klog.Errorf("Could not write response: %v", err)
		return err
	}
	return nil
}

func main() {
	klog.Info("Starting PVC Shrink Ray...")

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Could not get in-cluster config: %v", err)
	}

	client, err := snapshotclient.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Could not create snapshot client: %v", err)
	}

	http.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		handleMutate(w, r, client)
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:         ":8443",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	errChan := make(chan error, 1)
	go func() {
		klog.Infof("Listening on %s", server.Addr)
		if err := server.ListenAndServeTLS("/cert/server/certs/tls.crt", "/cert/server/certs/tls.key"); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
		close(errChan)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		klog.Infof("Received signal %s, shutting down...", sig)
	case err := <-errChan:
		klog.Fatalf("Server died: %v", err)
	}

	klog.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		klog.Errorf("Server shutdown error: %v", err)
	}
}
