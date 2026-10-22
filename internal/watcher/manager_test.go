package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ericeng/tuber/internal/config"
	"github.com/ericeng/tuber/internal/doit"
	"github.com/ericeng/tuber/internal/formatter"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestManagerEndToEndWithFakeClusterAndDoiTServer(t *testing.T) {
	var mu sync.Mutex
	var received []doit.CreateAnnotationRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req doit.CreateAnnotationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		mu.Lock()
		received = append(received, req)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(doit.AnnotationResponse{
			ID:        "ann-e2e",
			Content:   req.Content,
			Timestamp: req.Timestamp,
		})
	}))
	defer srv.Close()

	// Seed a pre-existing Deployment before starting Manager to verify cold-start suppression.
	existingDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "legacy-service",
			Namespace:  "production",
			UID:        types.UID("uid-legacy"),
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "legacy:v1"}},
				},
			},
		},
	}

	k8sClient := fake.NewSimpleClientset(existingDep)

	cfg, err := config.Parse([]byte(`
cluster:
  name: prod-eks-1
  provider: aws-eks
  region: us-east-1
watchNamespaces:
  - production
doit:
  baseURL: ` + srv.URL + `
  apiToken: test-api-key
  defaultReports:
    - rep-default
  defaultLabels:
    - lbl-default
`))
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	sink := doit.NewClient(cfg.DoiT, false)
	fmttr, err := formatter.New(cfg.DoiT)
	if err != nil {
		t.Fatalf("formatter.New() error: %v", err)
	}

	mgr, err := NewManager(cfg, k8sClient, sink, fmttr, nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()

	// Wait for Manager to become Ready.
	deadline := time.Now().Add(5 * time.Second)
	for !mgr.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for manager informer caches to sync")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify 0 annotations were sent for pre-existing Deployment.
	mu.Lock()
	initialCount := len(received)
	mu.Unlock()
	if initialCount != 0 {
		t.Fatalf("expected 0 annotations after initial cache sync, got %d", initialCount)
	}

	// 1. Create a Deployment in an UNWATCHED namespace ("kube-system") -> must be ignored.
	_, err = k8sClient.AppsV1().Deployments("kube-system").Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: "kube-system",
			UID:       types.UID("uid-coredns"),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating kube-system deployment: %v", err)
	}

	// 2. Create a new Deployment in watched namespace ("production") -> should emit Created.
	liveDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "billing-worker",
			Namespace:  "production",
			UID:        types.UID("uid-billing"),
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "billing:v1.0"}},
				},
			},
		},
	}
	_, err = k8sClient.AppsV1().Deployments("production").Create(ctx, liveDep, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating production deployment: %v", err)
	}

	// 3. Create a ReplicaSet-owned Pod in "production" -> should be suppressed by smart rollup.
	ownedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "billing-worker-abc-123",
			Namespace: "production",
			UID:       types.UID("uid-pod-owned"),
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "billing-worker-abc"},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: "billing:v1.0"}},
		},
	}
	_, err = k8sClient.CoreV1().Pods("production").Create(ctx, ownedPod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating owned pod: %v", err)
	}

	// 4. Transition the owned Pod into OOMKilled -> should emit OOMKilled even for owned Pod.
	oomPod := ownedPod.DeepCopy()
	oomPod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:         "worker",
			RestartCount: 1,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		},
	}
	_, err = k8sClient.CoreV1().Pods("production").UpdateStatus(ctx, oomPod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating owned pod status to OOMKilled: %v", err)
	}

	// 5. Create a standalone Pod (no OwnerReferences) in "production" -> should emit Created.
	_, err = k8sClient.CoreV1().Pods("production").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "migration-Bastion",
			Namespace: "production",
			UID:       types.UID("uid-pod-standalone"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Image: "alpine:3.20"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating standalone pod: %v", err)
	}

	// 6. Scale the Deployment in "production" from 2 -> 5 replicas -> should emit Scaled.
	scaledDep := liveDep.DeepCopy()
	scaledDep.Generation = 2
	scaledDep.Spec.Replicas = int32Ptr(5)
	_, err = k8sClient.AppsV1().Deployments("production").Update(ctx, scaledDep, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("scaling deployment: %v", err)
	}

	// 7. Roll out a new container image on the Deployment -> should emit RolledOut.
	rolledDep := scaledDep.DeepCopy()
	rolledDep.Generation = 3
	rolledDep.Spec.Template.Spec.Containers[0].Image = "billing:v2.0"
	_, err = k8sClient.AppsV1().Deployments("production").Update(ctx, rolledDep, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("rolling out deployment image: %v", err)
	}

	// 8. Create and complete a Job in "production" -> should emit Created and Completed.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cost-reconciler",
			Namespace: "production",
			UID:       types.UID("uid-job-1"),
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "job", Image: "reconciler:v2"}},
				},
			},
		},
	}
	_, err = k8sClient.BatchV1().Jobs("production").Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating job: %v", err)
	}

	completedJob := job.DeepCopy()
	completedJob.Status.Succeeded = 1
	completedJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	_, err = k8sClient.BatchV1().Jobs("production").UpdateStatus(ctx, completedJob, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating job status: %v", err)
	}

	// Wait for 7 expected annotations:
	// 1) Deployment Created
	// 2) Owned Pod OOMKilled (while Owned Pod Created was suppressed)
	// 3) Standalone Pod Created
	// 4) Deployment Scaled
	// 5) Deployment RolledOut
	// 6) Job Created
	// 7) Job Completed
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 7 || time.Now().After(waitDeadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("mgr.Start() returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 7 {
		t.Fatalf("expected 7 DoiT annotations, got %d: %+v", len(received), received)
	}

	wantSubstrings := []string{
		"[aws-eks/prod-eks-1] production/Deployment/billing-worker Created",
		"[aws-eks/prod-eks-1] production/Pod/billing-worker-abc-123 OOMKilled: container=worker exitCode=137 restarts=1",
		"[aws-eks/prod-eks-1] production/Pod/migration-Bastion Created: images=runner=alpine:3.20",
		"[aws-eks/prod-eks-1] production/Deployment/billing-worker Scaled: replicas 2 -> 5",
		"[aws-eks/prod-eks-1] production/Deployment/billing-worker RolledOut: images worker=billing:v1.0 -> worker=billing:v2.0",
		"[aws-eks/prod-eks-1] production/Job/cost-reconciler Created",
		"[aws-eks/prod-eks-1] production/Job/cost-reconciler Completed",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, got := range received {
			if strings.Contains(got.Content, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected annotation containing %q in received: %+v", want, received)
		}
	}
}
