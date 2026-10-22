package watcher

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ericethridge/tuber/internal/config"
	"github.com/ericethridge/tuber/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// Evaluator converts raw Kubernetes informer events into high-signal WorkloadEvents
// according to the configured WatchRules and smart workload rollup settings.
type Evaluator struct {
	cfg     *config.Config
	nowFunc func() time.Time
}

// NewEvaluator creates a workload event evaluator.
func NewEvaluator(cfg *config.Config) *Evaluator {
	return &Evaluator{
		cfg:     cfg,
		nowFunc: func() time.Time { return time.Now().UTC() },
	}
}

// EvaluateDeploymentAdd evaluates a newly observed Deployment.
func (e *Evaluator) EvaluateDeploymentAdd(dep *appsv1.Deployment, isInInitialList bool) []model.WorkloadEvent {
	if dep == nil || isInInitialList {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(dep.Annotations)
	if overrides.Ignore {
		return nil
	}
	rule, ok := e.matchRule("apps/v1", "Deployment", dep.Namespace, model.ActionCreated, dep.Labels)
	if !ok {
		return nil
	}

	rep := deploymentReplicas(dep)
	images := joinContainerImages(dep.Spec.Template.Spec.Containers)
	summary := fmt.Sprintf("replicas=%d images=%s", rep, images)
	stateKey := fmt.Sprintf("created:replicas=%d:images=%s", rep, images)

	return []model.WorkloadEvent{
		e.buildEvent(dep.ObjectMeta, "apps/v1", "Deployment", model.ActionCreated, summary, stateKey, overrides, rule),
	}
}

// EvaluateDeploymentUpdate compares old and new Deployment specs to detect Scaling or Rollout changes,
// ignoring routine status-only updates.
func (e *Evaluator) EvaluateDeploymentUpdate(oldDep, newDep *appsv1.Deployment) []model.WorkloadEvent {
	if oldDep == nil || newDep == nil {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(newDep.Annotations)
	if overrides.Ignore {
		return nil
	}

	var events []model.WorkloadEvent

	oldRep := deploymentReplicas(oldDep)
	newRep := deploymentReplicas(newDep)
	if oldRep != newRep {
		if rule, ok := e.matchRule("apps/v1", "Deployment", newDep.Namespace, model.ActionScaled, newDep.Labels); ok {
			images := joinContainerImages(newDep.Spec.Template.Spec.Containers)
			summary := fmt.Sprintf("replicas %d -> %d (images=%s)", oldRep, newRep, images)
			stateKey := fmt.Sprintf("scale:%d->%d", oldRep, newRep)
			events = append(events, e.buildEvent(newDep.ObjectMeta, "apps/v1", "Deployment", model.ActionScaled, summary, stateKey, overrides, rule))
		}
	}

	if diff := diffPodSpec(oldDep.Spec.Template.Spec, newDep.Spec.Template.Spec); diff != "" {
		if rule, ok := e.matchRule("apps/v1", "Deployment", newDep.Namespace, model.ActionRolledOut, newDep.Labels); ok {
			stateKey := fmt.Sprintf("rollout:%s", diff)
			events = append(events, e.buildEvent(newDep.ObjectMeta, "apps/v1", "Deployment", model.ActionRolledOut, diff, stateKey, overrides, rule))
		}
	}

	return events
}

// EvaluateDeploymentDelete evaluates a deleted Deployment (unwrapping cache tombstones if necessary).
func (e *Evaluator) EvaluateDeploymentDelete(obj any) []model.WorkloadEvent {
	raw := unwrapTombstone(obj)
	dep, ok := raw.(*appsv1.Deployment)
	if !ok || dep == nil {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(dep.Annotations)
	if overrides.Ignore {
		return nil
	}
	rule, ok := e.matchRule("apps/v1", "Deployment", dep.Namespace, model.ActionDeleted, dep.Labels)
	if !ok {
		return nil
	}
	summary := fmt.Sprintf("replicas=%d images=%s", deploymentReplicas(dep), joinContainerImages(dep.Spec.Template.Spec.Containers))
	return []model.WorkloadEvent{
		e.buildEvent(dep.ObjectMeta, "apps/v1", "Deployment", model.ActionDeleted, summary, "deleted", overrides, rule),
	}
}

// EvaluateJobAdd evaluates a newly created Job.
func (e *Evaluator) EvaluateJobAdd(job *batchv1.Job, isInInitialList bool) []model.WorkloadEvent {
	if job == nil || isInInitialList {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(job.Annotations)
	if overrides.Ignore {
		return nil
	}
	rule, ok := e.matchRule("batch/v1", "Job", job.Namespace, model.ActionCreated, job.Labels)
	if !ok {
		return nil
	}
	images := joinContainerImages(job.Spec.Template.Spec.Containers)
	summary := fmt.Sprintf("images=%s", images)
	return []model.WorkloadEvent{
		e.buildEvent(job.ObjectMeta, "batch/v1", "Job", model.ActionCreated, summary, "created", overrides, rule),
	}
}

// EvaluateJobUpdate detects edge transitions of a Job into Complete or Failed conditions.
func (e *Evaluator) EvaluateJobUpdate(oldJob, newJob *batchv1.Job) []model.WorkloadEvent {
	if oldJob == nil || newJob == nil {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(newJob.Annotations)
	if overrides.Ignore {
		return nil
	}

	var events []model.WorkloadEvent
	images := joinContainerImages(newJob.Spec.Template.Spec.Containers)

	if !hasJobCondition(oldJob, batchv1.JobComplete) && hasJobCondition(newJob, batchv1.JobComplete) {
		if rule, ok := e.matchRule("batch/v1", "Job", newJob.Namespace, model.ActionCompleted, newJob.Labels); ok {
			summary := fmt.Sprintf("succeeded=%d images=%s", newJob.Status.Succeeded, images)
			events = append(events, e.buildEvent(newJob.ObjectMeta, "batch/v1", "Job", model.ActionCompleted, summary, "completed", overrides, rule))
		}
	}

	if !hasJobCondition(oldJob, batchv1.JobFailed) && hasJobCondition(newJob, batchv1.JobFailed) {
		if rule, ok := e.matchRule("batch/v1", "Job", newJob.Namespace, model.ActionFailed, newJob.Labels); ok {
			reason := jobConditionReason(newJob, batchv1.JobFailed)
			summary := fmt.Sprintf("failed=%d reason=%s images=%s", newJob.Status.Failed, reason, images)
			events = append(events, e.buildEvent(newJob.ObjectMeta, "batch/v1", "Job", model.ActionFailed, summary, "failed", overrides, rule))
		}
	}

	return events
}

// EvaluatePodAdd evaluates a newly created Pod, suppressing owned Pods by default unless force-watched.
func (e *Evaluator) EvaluatePodAdd(pod *corev1.Pod, isInInitialList bool) []model.WorkloadEvent {
	if pod == nil || isInInitialList {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(pod.Annotations)
	if overrides.Ignore {
		return nil
	}

	var events []model.WorkloadEvent

	if !e.shouldSkipOwnedPod(pod, overrides) {
		if rule, ok := e.matchRule("v1", "Pod", pod.Namespace, model.ActionCreated, pod.Labels); ok {
			summary := fmt.Sprintf("images=%s", joinContainerImages(pod.Spec.Containers))
			events = append(events, e.buildEvent(pod.ObjectMeta, "v1", "Pod", model.ActionCreated, summary, "created", overrides, rule))
		}
	}

	if e.cfg.Rollup.TrackOOMKilledEnabled() {
		events = append(events, e.detectPodOOMEvents(nil, pod, overrides)...)
	}

	return events
}

// EvaluatePodUpdate detects critical container failure transitions such as OOMKilled.
func (e *Evaluator) EvaluatePodUpdate(oldPod, newPod *corev1.Pod) []model.WorkloadEvent {
	if newPod == nil {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(newPod.Annotations)
	if overrides.Ignore {
		return nil
	}
	if !e.cfg.Rollup.TrackOOMKilledEnabled() {
		return nil
	}
	return e.detectPodOOMEvents(oldPod, newPod, overrides)
}

// EvaluatePodDelete evaluates a deleted Pod, suppressing owned Pods by default unless force-watched.
func (e *Evaluator) EvaluatePodDelete(obj any) []model.WorkloadEvent {
	raw := unwrapTombstone(obj)
	pod, ok := raw.(*corev1.Pod)
	if !ok || pod == nil {
		return nil
	}
	overrides := model.ExtractAnnotationOverrides(pod.Annotations)
	if overrides.Ignore || e.shouldSkipOwnedPod(pod, overrides) {
		return nil
	}
	rule, ok := e.matchRule("v1", "Pod", pod.Namespace, model.ActionDeleted, pod.Labels)
	if !ok {
		return nil
	}
	summary := fmt.Sprintf("images=%s", joinContainerImages(pod.Spec.Containers))
	return []model.WorkloadEvent{
		e.buildEvent(pod.ObjectMeta, "v1", "Pod", model.ActionDeleted, summary, "deleted", overrides, rule),
	}
}

func (e *Evaluator) shouldSkipOwnedPod(pod *corev1.Pod, overrides model.AnnotationOverrides) bool {
	if overrides.ForceWatch {
		return false
	}
	return len(pod.OwnerReferences) > 0 && e.cfg.Rollup.IgnoreOwnedPodsEnabled()
}

func (e *Evaluator) detectPodOOMEvents(oldPod, newPod *corev1.Pod, overrides model.AnnotationOverrides) []model.WorkloadEvent {
	rule, ok := e.matchRule("v1", "Pod", newPod.Namespace, model.ActionOOMKilled, newPod.Labels)
	if !ok {
		return nil
	}

	var events []model.WorkloadEvent
	allNew := append([]corev1.ContainerStatus{}, newPod.Status.InitContainerStatuses...)
	allNew = append(allNew, newPod.Status.ContainerStatuses...)

	var allOld []corev1.ContainerStatus
	if oldPod != nil {
		allOld = append(allOld, oldPod.Status.InitContainerStatuses...)
		allOld = append(allOld, oldPod.Status.ContainerStatuses...)
	}

	for _, cs := range allNew {
		term := extractOOMTerminated(cs)
		if term == nil {
			continue
		}
		if oldPod != nil && hadSameOOMState(allOld, cs.Name, cs.RestartCount, term) {
			continue
		}
		summary := fmt.Sprintf("container=%s exitCode=%d restarts=%d", cs.Name, term.ExitCode, cs.RestartCount)
		stateKey := fmt.Sprintf("oom:%s:restarts=%d", cs.Name, cs.RestartCount)
		ev := e.buildEvent(newPod.ObjectMeta, "v1", "Pod", model.ActionOOMKilled, summary, stateKey, overrides, rule)
		// Container status updates don't bump metadata.generation on Pods; pin Generation=0 for stable dedup key.
		ev.Generation = 0
		events = append(events, ev)
	}

	return events
}

func extractOOMTerminated(cs corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
		return cs.State.Terminated
	}
	if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
		return cs.LastTerminationState.Terminated
	}
	return nil
}

func hadSameOOMState(oldStatuses []corev1.ContainerStatus, name string, restartCount int32, term *corev1.ContainerStateTerminated) bool {
	for _, oldCS := range oldStatuses {
		if oldCS.Name != name {
			continue
		}
		oldTerm := extractOOMTerminated(oldCS)
		if oldTerm == nil {
			return false
		}
		if oldCS.RestartCount == restartCount && oldTerm.FinishedAt.Equal(&term.FinishedAt) {
			return true
		}
	}
	return false
}

func (e *Evaluator) matchRule(apiVersion, kind, namespace string, action model.Action, objLabels map[string]string) (*config.WatchRule, bool) {
	for i := range e.cfg.Rules {
		rule := &e.cfg.Rules[i]
		if !strings.EqualFold(rule.APIVersion, apiVersion) || !strings.EqualFold(rule.Kind, kind) {
			continue
		}
		if !rule.AllowsNamespace(namespace) || !rule.AllowsAction(action) {
			continue
		}
		if strings.TrimSpace(rule.LabelSelector) != "" {
			sel, err := labels.Parse(rule.LabelSelector)
			if err != nil || !sel.Matches(labels.Set(objLabels)) {
				continue
			}
		}
		return rule, true
	}
	return nil, false
}

func (e *Evaluator) buildEvent(
	meta metav1.ObjectMeta,
	apiVersion, kind string,
	action model.Action,
	summary, stateKey string,
	overrides model.AnnotationOverrides,
	rule *config.WatchRule,
) model.WorkloadEvent {
	var ruleReports, ruleLabels []string
	if rule != nil {
		ruleReports = rule.Reports
		ruleLabels = rule.Labels
	}
	return model.WorkloadEvent{
		ClusterName:   e.cfg.Cluster.Name,
		CloudProvider: e.cfg.Cluster.Provider,
		Region:        e.cfg.Cluster.Region,
		Namespace:     meta.Namespace,
		Kind:          kind,
		APIVersion:    apiVersion,
		Name:          meta.Name,
		UID:           string(meta.UID),
		Action:        action,
		Summary:       summary,
		Timestamp:     e.nowFunc(),
		Generation:    meta.Generation,
		StateKey:      stateKey,
		ReportIDs:     overrides.Reports,
		LabelIDs:      overrides.Labels,
		RuleReportIDs: ruleReports,
		RuleLabelIDs:  ruleLabels,
	}
}

func unwrapTombstone(obj any) any {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return tombstone.Obj
	}
	if tombstonePtr, ok := obj.(*cache.DeletedFinalStateUnknown); ok && tombstonePtr != nil {
		return tombstonePtr.Obj
	}
	return obj
}

func deploymentReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas == nil {
		return 1
	}
	return *dep.Spec.Replicas
}

func joinContainerImages(containers []corev1.Container) string {
	if len(containers) == 0 {
		return "none"
	}
	imgs := make([]string, 0, len(containers))
	for _, c := range containers {
		imgs = append(imgs, fmt.Sprintf("%s=%s", c.Name, c.Image))
	}
	return strings.Join(imgs, ",")
}

func diffPodSpec(oldSpec, newSpec corev1.PodSpec) string {
	var changes []string
	oldImgs := joinContainerImages(oldSpec.Containers)
	newImgs := joinContainerImages(newSpec.Containers)
	if oldImgs != newImgs {
		changes = append(changes, fmt.Sprintf("images %s -> %s", oldImgs, newImgs))
	}

	if !containersResourcesEqual(oldSpec.Containers, newSpec.Containers) {
		changes = append(changes, "resources updated")
	}

	return strings.Join(changes, "; ")
}

func containersResourcesEqual(a, b []corev1.Container) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i].Resources, b[i].Resources) {
			return false
		}
	}
	return true
}

func hasJobCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobConditionReason(job *batchv1.Job, condType batchv1.JobConditionType) string {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue && c.Reason != "" {
			return c.Reason
		}
	}
	return "unknown"
}
