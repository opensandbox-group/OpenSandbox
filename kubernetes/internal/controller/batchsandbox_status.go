// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

type runtimeView struct {
	status                   *sandboxv1alpha1.BatchSandboxStatus
	endpointIPs              []string
	restartDetectionBaseline map[string]int64
	resumeCompleted          bool
}

func setConditionInStatus(
	status *sandboxv1alpha1.BatchSandboxStatus,
	conditionType sandboxv1alpha1.BatchSandboxConditionType,
	conditionStatus string,
	reason string,
	message string,
) {
	filtered := make([]sandboxv1alpha1.BatchSandboxCondition, 0, len(status.Conditions))
	found := false
	for _, cond := range status.Conditions {
		if cond.Type != conditionType {
			filtered = append(filtered, cond)
			continue
		}
		found = true
		if conditionStatus == sandboxv1alpha1.ConditionFalse {
			continue
		}
		if cond.Status == conditionStatus && cond.Reason == reason && cond.Message == message {
			filtered = append(filtered, cond)
			continue
		}
		cond.Status = conditionStatus
		cond.Reason = reason
		cond.Message = message
		cond.LastTransitionTime = ptr.To(metav1.Now())
		filtered = append(filtered, cond)
	}
	if !found && conditionStatus == sandboxv1alpha1.ConditionTrue {
		filtered = append(filtered, sandboxv1alpha1.BatchSandboxCondition{
			Type:               conditionType,
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: ptr.To(metav1.Now()),
		})
	}
	status.Conditions = filtered
}

func applyBatchSandboxPhaseConditions(status *sandboxv1alpha1.BatchSandboxStatus) {
	switch status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePending:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionFalse, "Creating", "Sandbox is being created")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionTrue, "Creating", "Sandbox is being created")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.BatchSandboxPhaseSucceed:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionTrue, "PodsReady", "Sandbox is running")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionFalse, "", "")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.BatchSandboxPhasePausing:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionFalse, "PauseInProgress", "Pausing sandbox")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionTrue, "PauseInProgress", "Pausing sandbox")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.BatchSandboxPhasePaused:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionFalse, "Paused", "Sandbox is paused")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionFalse, "", "")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionTrue, "Paused", "Sandbox is paused")
	case sandboxv1alpha1.BatchSandboxPhaseResuming:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionFalse, "ResumeInProgress", "Resuming sandbox")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionTrue, "ResumeInProgress", "Resuming sandbox")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.BatchSandboxPhaseFailed:
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionReady, sandboxv1alpha1.ConditionFalse, "Failed", "Sandbox is unavailable")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionProgressing, sandboxv1alpha1.ConditionFalse, "", "")
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPaused, sandboxv1alpha1.ConditionFalse, "", "")
	}
}

func getPodFailureReasonAndMessage(pod *corev1.Pod, readySince *metav1.Time) (string, string, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
			return cs.State.Waiting.Reason, fmt.Sprintf("Pod %s: %s - %s", pod.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message), true
		}
	}

	if readySince == nil {
		return "", "", false
	}
	if len(pod.Spec.Containers) == 0 {
		return "", "", false
	}
	// OpenSandbox treats the first regular container as the stateful sandbox workload;
	// later containers are supporting sidecars such as egress.
	mainContainerName := pod.Spec.Containers[0].Name
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == mainContainerName {
			terminated := cs.State.Terminated
			if terminated == nil && cs.RestartCount > 0 {
				terminated = cs.LastTerminationState.Terminated
			}
			if terminated == nil || !terminated.FinishedAt.After(readySince.Time) {
				return "", "", false
			}
			reason := terminated.Reason
			if reason == "" {
				reason = "ContainerRestarted"
			}
			return reason, fmt.Sprintf("Pod %s container %s terminated after the sandbox became ready", pod.Name, cs.Name), true
		}
	}
	return "", "", false
}

type restartDetectionBaseline struct {
	readySince          *metav1.Time
	previousEndpointIPs map[string]struct{}
	perPod              map[string]int64
	hasPerPodBaseline   bool
	detectRestarts      bool
	desiredPerPod       map[string]int64
}

func endpointMembership(endpointIPs []string) map[string]struct{} {
	membership := make(map[string]struct{}, len(endpointIPs))
	for _, ip := range endpointIPs {
		if ip != "" {
			membership[ip] = struct{}{}
		}
	}
	return membership
}

func (b restartDetectionBaseline) forPod(pod *corev1.Pod) *metav1.Time {
	if !b.detectRestarts {
		return nil
	}
	if baseline, exists := b.perPod[podRestartBaselineKey(pod)]; exists && baseline > 0 {
		persisted := metav1.NewTime(time.Unix(0, baseline))
		return &persisted
	}
	if b.hasPerPodBaseline || b.readySince == nil || pod.Status.PodIP == "" {
		return nil
	}
	if _, existedWhenReady := b.previousEndpointIPs[pod.Status.PodIP]; !existedWhenReady {
		return nil
	}
	if !pod.CreationTimestamp.IsZero() && pod.CreationTimestamp.After(b.readySince.Time) {
		return nil
	}
	return b.readySince
}

func podRestartBaselineKey(pod *corev1.Pod) string {
	if pod.UID != "" {
		return string(pod.UID)
	}
	// Pods returned by the API server always have a UID. Falling back to the
	// name keeps direct unit fixtures useful without weakening production keys.
	return pod.Name
}

func buildRestartDetectionBaseline(batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod, endpointIPs []string) restartDetectionBaseline {
	status := batchSbx.Status
	baseline := restartDetectionBaseline{
		detectRestarts: status.Phase == sandboxv1alpha1.BatchSandboxPhaseSucceed,
		desiredPerPod:  make(map[string]int64, len(pods)),
	}

	if baseline.detectRestarts {
		for i := range status.Conditions {
			condition := &status.Conditions[i]
			if condition.Type == sandboxv1alpha1.BatchSandboxConditionReady && condition.Status == sandboxv1alpha1.ConditionTrue &&
				condition.LastTransitionTime != nil && !condition.LastTransitionTime.IsZero() {
				baseline.readySince = condition.LastTransitionTime
				break
			}
		}
	}

	if batchSbx.Annotations != nil {
		if raw := batchSbx.Annotations[AnnoRestartBaselineKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &baseline.perPod); err == nil {
				baseline.hasPerPodBaseline = true
			} else {
				baseline.perPod = nil
			}
			if baseline.hasPerPodBaseline && baseline.perPod == nil {
				baseline.perPod = map[string]int64{}
			}
		}
	}

	var previousIPs []string
	if batchSbx.Annotations != nil && json.Unmarshal([]byte(batchSbx.Annotations[AnnotationSandboxEndpoints]), &previousIPs) == nil {
		baseline.previousEndpointIPs = endpointMembership(previousIPs)
	}

	now := time.Now().UnixNano()
	for i, pod := range pods {
		if i >= len(endpointIPs) || endpointIPs[i] == "" {
			continue
		}
		key := podRestartBaselineKey(pod)
		if key == "" {
			continue
		}
		if persisted, exists := baseline.perPod[key]; exists && persisted > 0 {
			baseline.desiredPerPod[key] = persisted
			continue
		}
		// Migrate Pods that were already exposed before this annotation existed
		// to the original Ready transition. New and replacement Pods start at the
		// timestamp that will be atomically published with their endpoint.
		if legacy := baseline.forPod(pod); legacy != nil {
			baseline.desiredPerPod[key] = legacy.UnixNano()
		} else {
			baseline.desiredPerPod[key] = now
		}
	}
	return baseline
}

type podFailureSummary struct {
	observed      int
	failed        int
	primaryReason string
	samplePod     string
}

func summarizePodFailures(pods []*corev1.Pod, baseline *restartDetectionBaseline) (podFailureSummary, bool) {
	summary := podFailureSummary{observed: len(pods)}
	reasonCounts := make(map[string]int)
	firstPodByReason := make(map[string]string)
	primaryCount := 0

	for _, pod := range pods {
		var readySince *metav1.Time
		if baseline != nil {
			readySince = baseline.forPod(pod)
		}
		reason, _, failed := getPodFailureReasonAndMessage(pod, readySince)
		if !failed {
			continue
		}

		summary.failed++
		if _, exists := firstPodByReason[reason]; !exists {
			firstPodByReason[reason] = pod.Name
		}
		reasonCounts[reason]++
		if reasonCounts[reason] > primaryCount {
			primaryCount = reasonCounts[reason]
			summary.primaryReason = reason
			summary.samplePod = firstPodByReason[reason]
		}
	}

	return summary, summary.failed > 0
}

func (s podFailureSummary) message(duringResume bool) string {
	scope := "observed pods failed"
	if duringResume {
		scope = "observed pods failed during resume"
	}
	return fmt.Sprintf("%d/%d %s; primary reason=%s; sample pod=%s", s.failed, s.observed, scope, s.primaryReason, s.samplePod)
}

func buildRuntimeView(batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) runtimeView {
	view := runtimeView{}
	newStatus := batchSbx.Status.DeepCopy()
	newStatus.ObservedGeneration = batchSbx.Generation
	newStatus.Replicas = 0
	newStatus.Allocated = 0
	newStatus.Ready = 0

	ipList := make([]string, len(pods))
	for i, pod := range pods {
		newStatus.Replicas++
		if utils.IsAssigned(pod) {
			newStatus.Allocated++
			ipList[i] = pod.Status.PodIP
		}
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && utils.IsPodReady(pod) {
			newStatus.Ready++
		}
	}

	baseline := buildRestartDetectionBaseline(batchSbx, pods, ipList)
	switch batchSbx.Status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePausing, sandboxv1alpha1.BatchSandboxPhasePaused:
		// Keep lifecycle-owned stable phases unchanged.
	case sandboxv1alpha1.BatchSandboxPhaseResuming:
		applyResumingRuntimePhase(newStatus, pods)
	default:
		applySteadyRuntimePhase(batchSbx, newStatus, pods, &baseline)
	}

	applyBatchSandboxPhaseConditions(newStatus)
	if batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhaseSucceed &&
		newStatus.Phase == sandboxv1alpha1.BatchSandboxPhaseSucceed {
		alignRestartBaselinesWithReadyTransition(newStatus, baseline.desiredPerPod)
	}

	view.status = newStatus
	view.endpointIPs = ipList
	view.restartDetectionBaseline = baseline.desiredPerPod
	view.resumeCompleted = batchSbx.Status.Phase == sandboxv1alpha1.BatchSandboxPhaseResuming && newStatus.Phase == sandboxv1alpha1.BatchSandboxPhaseSucceed
	return view
}

func alignRestartBaselinesWithReadyTransition(status *sandboxv1alpha1.BatchSandboxStatus, baselines map[string]int64) {
	for i := range status.Conditions {
		condition := &status.Conditions[i]
		if condition.Type != sandboxv1alpha1.BatchSandboxConditionReady || condition.Status != sandboxv1alpha1.ConditionTrue ||
			condition.LastTransitionTime == nil || condition.LastTransitionTime.IsZero() {
			continue
		}
		for key := range baselines {
			baselines[key] = condition.LastTransitionTime.UnixNano()
		}
		return
	}
}

func applyResumingRuntimePhase(status *sandboxv1alpha1.BatchSandboxStatus, pods []*corev1.Pod) {
	if summary, hasFailures := summarizePodFailures(pods, nil); hasFailures {
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, summary.primaryReason, summary.message(true))
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionTrue, summary.primaryReason, summary.message(false))
		status.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
		return
	}
	if status.Ready > 0 {
		status.Phase = sandboxv1alpha1.BatchSandboxPhaseSucceed
		setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionFalse, "", "")
	}
}

func applySteadyRuntimePhase(batchSbx *sandboxv1alpha1.BatchSandbox, status *sandboxv1alpha1.BatchSandboxStatus, pods []*corev1.Pod, baseline *restartDetectionBaseline) {
	if summary, hasFailures := summarizePodFailures(pods, baseline); hasFailures {
		if batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhaseFailed {
			setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionTrue, summary.primaryReason, summary.message(false))
			status.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
		}
		return
	}

	if status.Phase == sandboxv1alpha1.BatchSandboxPhaseFailed {
		return
	}

	setConditionInStatus(status, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionFalse, "", "")
	if status.Ready > 0 {
		status.Phase = sandboxv1alpha1.BatchSandboxPhaseSucceed
		return
	}
	status.Phase = sandboxv1alpha1.BatchSandboxPhasePending
}

// isInitialUnallocatedSandbox returns true when the sandbox has just been created
// and no pods have been allocated yet. In this case we skip writing the initial
// Pending status — the next reconcile after allocation will write Succeed directly.
func isInitialUnallocatedSandbox(batchSbx *sandboxv1alpha1.BatchSandbox, view runtimeView) bool {
	return view.status.Replicas == 0 && batchSbx.Status.Phase == "" &&
		batchSbx.Spec.Replicas != nil && *batchSbx.Spec.Replicas > 0
}

func (r *BatchSandboxReconciler) persistRuntimeView(
	ctx context.Context,
	batchSbx *sandboxv1alpha1.BatchSandbox,
	view runtimeView,
) (time.Duration, []error) {
	var aggErrors []error
	log := logf.FromContext(ctx)
	if isInitialUnallocatedSandbox(batchSbx, view) {
		return 0, aggErrors
	}
	statusChanged := !equality.Semantic.DeepEqual(*view.status, batchSbx.Status)
	metadataChanged := runtimeMetadataNeedsPatch(batchSbx, view)
	if statusChanged || metadataChanged {
		// Skip redundant status writes caused by informer cache lag: if we recently
		// patched status but the informer hasn't seen the new RV yet, the runtime
		// view may also contain stale endpoint baselines. Allow a 10s safety valve
		// in case the cache never catches up.
		if satisfied, dur := r.StatusRVExpectation.IsSatisfied(batchSbx); !satisfied {
			if dur < 10*time.Second {
				log.Info("Skipping runtime view update: informer cache is stale", "unsatisfiedDuration", dur.String())
				return time.Second, aggErrors
			}
			log.Info("Proceeding with runtime view update despite stale cache (timeout exceeded)", "unsatisfiedDuration", dur.String())
			// Fetch the latest object so lifecycle conditions (PauseFailed/ResumeFailed)
			// and restart baselines are not overwritten by the stale cache.
			latest := &sandboxv1alpha1.BatchSandbox{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}, latest); err == nil {
				batchSbx = latest
				preservePersistedRestartBaselines(batchSbx, view.restartDetectionBaseline)
				statusChanged = !equality.Semantic.DeepEqual(*view.status, batchSbx.Status)
				metadataChanged = runtimeMetadataNeedsPatch(batchSbx, view)
			}
		}
	}
	if metadataChanged {
		if err := r.patchBatchSandboxRuntimeMetadata(ctx, batchSbx, view); err != nil {
			aggErrors = append(aggErrors, err)
		}
	}
	if statusChanged {
		if err := r.updateStatus(ctx, batchSbx, view.status); err != nil {
			aggErrors = append(aggErrors, err)
			return 0, aggErrors
		}
	}

	if view.status.Phase == sandboxv1alpha1.BatchSandboxPhaseSucceed {
		if err := r.deleteInternalPauseSnapshot(ctx, batchSbx); err != nil {
			log.Error(err, "Failed to delete SandboxSnapshot after successful resume")
			aggErrors = append(aggErrors, err)
		}
	}
	return 0, aggErrors
}

func preservePersistedRestartBaselines(batchSbx *sandboxv1alpha1.BatchSandbox, desired map[string]int64) {
	if desired == nil || batchSbx.Annotations == nil {
		return
	}
	persisted := map[string]int64{}
	if json.Unmarshal([]byte(batchSbx.Annotations[AnnoRestartBaselineKey]), &persisted) != nil {
		return
	}
	for key := range desired {
		if baseline, exists := persisted[key]; exists && baseline > 0 {
			desired[key] = baseline
		}
	}
}

func runtimeMetadataNeedsPatch(batchSbx *sandboxv1alpha1.BatchSandbox, view runtimeView) bool {
	endpointRaw, _ := json.Marshal(view.endpointIPs)
	_, endpointExists := batchSbx.Annotations[AnnotationSandboxEndpoints]
	endpointChanged := batchSbx.Annotations[AnnotationSandboxEndpoints] != string(endpointRaw)
	if !endpointExists && string(endpointRaw) == "[]" {
		endpointChanged = false
	}
	if view.restartDetectionBaseline == nil {
		return endpointChanged
	}
	baselineRaw, _ := json.Marshal(view.restartDetectionBaseline)
	_, baselineExists := batchSbx.Annotations[AnnoRestartBaselineKey]
	baselineChanged := batchSbx.Annotations[AnnoRestartBaselineKey] != string(baselineRaw)
	if !endpointExists && !baselineExists && string(endpointRaw) == "[]" && len(view.restartDetectionBaseline) == 0 {
		baselineChanged = false
	}
	return endpointChanged || baselineChanged
}

func (r *BatchSandboxReconciler) patchBatchSandboxRuntimeMetadata(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, view runtimeView) error {
	endpointRaw, _ := json.Marshal(view.endpointIPs)
	annotations := map[string]string{AnnotationSandboxEndpoints: string(endpointRaw)}
	if view.restartDetectionBaseline != nil {
		baselineRaw, _ := json.Marshal(view.restartDetectionBaseline)
		annotations[AnnoRestartBaselineKey] = string(baselineRaw)
	}
	log := logf.FromContext(ctx)
	patchData, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	})
	log.Info("Patching BatchSandbox runtime metadata", "resourceVersion", batchSbx.ResourceVersion, "patchData", string(patchData))
	obj := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: batchSbx.Namespace, Name: batchSbx.Name}}
	if err := r.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData)); err != nil {
		return err
	}
	// Prevent a stale informer view from regenerating a later baseline and
	// overwriting the timestamp that was atomically exposed with the endpoint.
	r.StatusRVExpectation.Expect(obj)
	return nil
}

func (r *BatchSandboxReconciler) updateStatus(ctx context.Context, batchSandbox *sandboxv1alpha1.BatchSandbox, newStatus *sandboxv1alpha1.BatchSandboxStatus) error {
	log := logf.FromContext(ctx)
	mergedStatus := newStatus.DeepCopy()
	mergedStatus.Conditions = mergeLifecycleConditions(mergedStatus.Conditions, batchSandbox.Status.Conditions)
	patchData, err := json.Marshal(map[string]any{"status": mergedStatus})
	if err != nil {
		return fmt.Errorf("failed to marshal status patch: %w", err)
	}
	log.Info("Patching BatchSandbox status", "resourceVersion", batchSandbox.ResourceVersion, "phase", mergedStatus.Phase, "patchData", string(patchData))
	obj := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: batchSandbox.Namespace, Name: batchSandbox.Name}}
	if err := r.Status().Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData)); err != nil {
		return err
	}
	r.StatusRVExpectation.Expect(obj)
	return nil
}

func mergeLifecycleConditions(
	desired []sandboxv1alpha1.BatchSandboxCondition,
	latest []sandboxv1alpha1.BatchSandboxCondition,
) []sandboxv1alpha1.BatchSandboxCondition {
	merged := append([]sandboxv1alpha1.BatchSandboxCondition(nil), desired...)
	hasCondition := make(map[sandboxv1alpha1.BatchSandboxConditionType]struct{}, len(desired))
	for _, cond := range desired {
		hasCondition[cond.Type] = struct{}{}
	}
	for _, cond := range latest {
		if !isLifecycleOwnedCondition(cond.Type) {
			continue
		}
		if _, exists := hasCondition[cond.Type]; exists {
			continue
		}
		merged = append(merged, cond)
	}
	return merged
}

func isLifecycleOwnedCondition(conditionType sandboxv1alpha1.BatchSandboxConditionType) bool {
	switch conditionType {
	case sandboxv1alpha1.BatchSandboxConditionPauseFailed,
		sandboxv1alpha1.BatchSandboxConditionResumeFailed:
		return true
	default:
		return false
	}
}
