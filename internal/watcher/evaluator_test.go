package watcher

import (
	"testing"

	"github.com/ericethridge/tuber/internal/config"
	"github.com/ericethridge/tuber/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

func newTestEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	cfg, err := config.Parse([]byte(`
cluster:
  name: test-gke
  provider: gcp-gke
  region: us-central1
watchNamespaces:
  - production
doit:
  apiToken: test-token
`))
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}
	return NewEvaluator(cfg)
}

func int32Ptr(v int32) *int32 { return &v }

func TestEvaluateDeploymentLifecycleAndNoiseSuppression(t *testing.T) {
	ev := newTestEvaluator(t)

	baseDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "checkout-api",
			Namespace:  "production",
			UID:        types.UID("dep-uid-1"),
			Generation: 1,
			Annotations: map[string]string{
				model.AnnotationReports: "rep-custom-1",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "api", Image: "gcr.io/acme/checkout:v1.0.0"},
					},
				},
			},
		},
	}

	// 1. Initial list must be suppressed (cold-start safety).
	if got := ev.EvaluateDeploymentAdd(baseDep, true); len(got) != 0 {
		t.Fatalf("expected 0 events when isInInitialList=true, got %d", len(got))
	}

	// 2. Live creation emits Created.
	created := ev.EvaluateDeploymentAdd(baseDep, false)
	if len(created) != 1 || created[0].Action != model.ActionCreated {
		t.Fatalf("expected 1 Created event, got %+v", created)
	}
	if len(created[0].ReportIDs) != 1 || created[0].ReportIDs[0] != "rep-custom-1" {
		t.Errorf("expected ReportIDs=[rep-custom-1], got %v", created[0].ReportIDs)
	}

	// 3. Status-only update emits 0 events.
	statusUpdated := baseDep.DeepCopy()
	statusUpdated.Status.ReadyReplicas = 3
	if got := ev.EvaluateDeploymentUpdate(baseDep, statusUpdated); len(got) != 0 {
		t.Fatalf("expected 0 events on status-only Deployment update, got %+v", got)
	}

	// 4. Replica change emits Scaled.
	scaledDep := baseDep.DeepCopy()
	scaledDep.Generation = 2
	scaledDep.Spec.Replicas = int32Ptr(8)
	scaledEvents := ev.EvaluateDeploymentUpdate(baseDep, scaledDep)
	if len(scaledEvents) != 1 || scaledEvents[0].Action != model.ActionScaled {
		t.Fatalf("expected 1 Scaled event, got %+v", scaledEvents)
	}

	// 5. Image change emits RolledOut.
	rolledDep := scaledDep.DeepCopy()
	rolledDep.Generation = 3
	rolledDep.Spec.Template.Spec.Containers[0].Image = "gcr.io/acme/checkout:v1.1.0"
	rolloutEvents := ev.EvaluateDeploymentUpdate(scaledDep, rolledDep)
	if len(rolloutEvents) != 1 || rolloutEvents[0].Action != model.ActionRolledOut {
		t.Fatalf("expected 1 RolledOut event on image update, got %+v", rolloutEvents)
	}

	// 6. Resource request change emits RolledOut.
	resDep := rolledDep.DeepCopy()
	resDep.Generation = 4
	resDep.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	resEvents := ev.EvaluateDeploymentUpdate(rolledDep, resDep)
	if len(resEvents) != 1 || resEvents[0].Action != model.ActionRolledOut {
		t.Fatalf("expected 1 RolledOut event on resource change, got %+v", resEvents)
	}

	// 7. Delete wrapped in DeletedFinalStateUnknown tombstone emits Deleted.
	tombstone := cache.DeletedFinalStateUnknown{
		Key: "production/checkout-api",
		Obj: resDep,
	}
	deleted := ev.EvaluateDeploymentDelete(tombstone)
	if len(deleted) != 1 || deleted[0].Action != model.ActionDeleted {
		t.Fatalf("expected 1 Deleted event from tombstone, got %+v", deleted)
	}
}

func TestEvaluateJobTransitions(t *testing.T) {
	ev := newTestEvaluator(t)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-etl",
			Namespace: "production",
			UID:       types.UID("job-1"),
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "etl", Image: "etl:v1"}},
				},
			},
		},
	}

	if got := ev.EvaluateJobAdd(job, false); len(got) != 1 || got[0].Action != model.ActionCreated {
		t.Fatalf("expected Job Created event, got %+v", got)
	}

	completedJob := job.DeepCopy()
	completedJob.Status.Succeeded = 1
	completedJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	got := ev.EvaluateJobUpdate(job, completedJob)
	if len(got) != 1 || got[0].Action != model.ActionCompleted {
		t.Fatalf("expected Job Completed event, got %+v", got)
	}

	// Subsequent update with Complete still True should not re-trigger.
	if gotAgain := ev.EvaluateJobUpdate(completedJob, completedJob); len(gotAgain) != 0 {
		t.Fatalf("expected 0 events when Job Complete condition did not transition, got %+v", gotAgain)
	}
}

func TestEvaluatePodOwnedSuppressionAndOOMKilled(t *testing.T) {
	ev := newTestEvaluator(t)

	ownedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-api-7f8b9-abcde",
			Namespace: "production",
			UID:       types.UID("pod-owned-1"),
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "checkout-api-7f8b9"},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "api", Image: "checkout:v1"}},
		},
	}

	// Owned Pod creation & deletion are suppressed by default.
	if got := ev.EvaluatePodAdd(ownedPod, false); len(got) != 0 {
		t.Fatalf("expected owned Pod creation to be suppressed, got %+v", got)
	}
	if got := ev.EvaluatePodDelete(ownedPod); len(got) != 0 {
		t.Fatalf("expected owned Pod deletion to be suppressed, got %+v", got)
	}

	// However, OOMKilled on an owned Pod IS captured!
	oomPod := ownedPod.DeepCopy()
	oomPod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:         "api",
			RestartCount: 1,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		},
	}
	oomEvents := ev.EvaluatePodUpdate(ownedPod, oomPod)
	if len(oomEvents) != 1 || oomEvents[0].Action != model.ActionOOMKilled {
		t.Fatalf("expected 1 OOMKilled event on owned Pod, got %+v", oomEvents)
	}

	// Standalone Pod (no OwnerReferences) emits Created and Deleted.
	standalonePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "debug-bastion",
			Namespace: "production",
			UID:       types.UID("pod-standalone-1"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "shell", Image: "alpine:3.20"}},
		},
	}
	if got := ev.EvaluatePodAdd(standalonePod, false); len(got) != 1 || got[0].Action != model.ActionCreated {
		t.Fatalf("expected standalone Pod Created event, got %+v", got)
	}
	if got := ev.EvaluatePodDelete(standalonePod); len(got) != 1 || got[0].Action != model.ActionDeleted {
		t.Fatalf("expected standalone Pod Deleted event, got %+v", got)
	}
}
