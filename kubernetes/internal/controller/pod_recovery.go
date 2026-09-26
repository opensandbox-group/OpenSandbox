// Copyright 2026 The OpenSandbox Authors
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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
)

const (
	defaultPodRecoveryStuckThreshold = time.Minute
	defaultPodRecoveryMaxAttempts    = 3
)

// Provisioning failure conditions. The recovery loop is generic; new
// conditions plug in as additional detectors.
const (
	recoveryConditionImagePull        = "ImagePull"
	recoveryConditionKubeletAdmission = "KubeletAdmission"
)

// Skip reasons for the podRecoverySkips metric.
const (
	recoverySkipPermanentFailure = "permanent_failure"
	recoverySkipBudgetExhausted  = "budget_exhausted"
)

var (
	podRecoveryReplacements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "opensandbox_pod_recovery_replacements_total",
		Help: "Stuck provisioning Pods replaced during initial startup, by condition.",
	}, []string{"condition"})
	podRecoverySkips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "opensandbox_pod_recovery_skips_total",
		Help: "Stuck provisioning Pods detected but not replaced, by condition and reason.",
	}, []string{"condition", "reason"})
)

func init() {
	metrics.Registry.MustRegister(podRecoveryReplacements, podRecoverySkips)
}

// provisioningFailure describes a pod stuck in a provisioning failure.
type provisioningFailure struct {
	Condition string
	Reason    string
	Message   string
	// Permanent failures (a broken image) are reported but never replaced.
	Permanent bool
}

// recoverableImagePullReasons are node-local image pull waiting states.
// CreateContainerConfigError and terminal failures are excluded on purpose.
var recoverableImagePullReasons = sets.New("ImagePullBackOff", "ErrImagePull")

// Kubelet admission rejection reasons a replacement can recover. The "OutOf"
// prefix covers OutOfcpu / OutOfmemory / OutOfephemeral-storage / OutOfpods.
var recoverableAdmissionReasons = sets.New(
	"NodeNotSchedulable",
	"KubeletNotReady",
	"UnexpectedAdmissionError",
	"Evicted",
)

func isRecoverableAdmissionFailure(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodFailed {
		return false
	}
	reason := pod.Status.Reason
	return strings.HasPrefix(reason, "OutOf") || recoverableAdmissionReasons.Has(reason)
}

// permanentImagePullFailureMarkers are kubelet pull error message fragments
// that indicate the image itself cannot be pulled from any node; unmatched
// messages fall through to the replacement path.
var permanentImagePullFailureMarkers = []string{
	"manifest unknown",
	"manifest for",
	"not found",
	"does not exist",
	"pull access denied",
	"authentication required",
	"unauthorized",
	"forbidden",
	"access denied",
	"permission denied",
}

func isPermanentImagePullFailure(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range permanentImagePullFailureMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// detectProvisioningFailure reports whether the pod is stuck in a tracked
// provisioning failure, for init and regular containers alike.
func detectProvisioningFailure(pod *corev1.Pod) (provisioningFailure, bool) {
	if pod.DeletionTimestamp != nil {
		return provisioningFailure{}, false
	}
	if isRecoverableAdmissionFailure(pod) {
		return provisioningFailure{
			Condition: recoveryConditionKubeletAdmission,
			Reason:    pod.Status.Reason,
			Message:   pod.Status.Message,
		}, true
	}
	for i := range pod.Status.InitContainerStatuses {
		if failure, ok := imagePullWaitingFailure(&pod.Status.InitContainerStatuses[i]); ok {
			return failure, true
		}
	}
	for i := range pod.Status.ContainerStatuses {
		if failure, ok := imagePullWaitingFailure(&pod.Status.ContainerStatuses[i]); ok {
			return failure, true
		}
	}
	return provisioningFailure{}, false
}

func imagePullWaitingFailure(status *corev1.ContainerStatus) (provisioningFailure, bool) {
	waiting := status.State.Waiting
	if waiting == nil || !recoverableImagePullReasons.Has(waiting.Reason) {
		return provisioningFailure{}, false
	}
	return provisioningFailure{
		Condition: recoveryConditionImagePull,
		Reason:    waiting.Reason,
		Message:   waiting.Message,
		Permanent: isPermanentImagePullFailure(waiting.Message),
	}, true
}

// podRecoveryBudget is the per-sandbox replacement budget. A generation
// mismatch (template update) resets it; so does eviction or a controller
// restart, which only allow a fresh bounded round of attempts. All access is
// under the store mutex.
type podRecoveryBudget struct {
	gen   int64
	count int
}

// The store is bounded: stale entries are removed on sandbox phase changes,
// and once podRecoveryBudgetMaxEntries is reached a random entry is evicted.
// Eviction only resets a budget, so it is always safe.
const podRecoveryBudgetMaxEntries = 10000

type podRecoveryBudgetStore struct {
	mu      sync.Mutex
	budgets map[string]*podRecoveryBudget
}

var podRecoveryBudgets = &podRecoveryBudgetStore{budgets: map[string]*podRecoveryBudget{}}

func (s *podRecoveryBudgetStore) load(key string, generation int64) *podRecoveryBudget {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.budgets[key]
	if !ok {
		b = &podRecoveryBudget{gen: generation}
		s.budgets[key] = b
		if len(s.budgets) > podRecoveryBudgetMaxEntries {
			for stale := range s.budgets {
				if stale != key {
					delete(s.budgets, stale)
					break
				}
			}
		}
	}
	if b.gen != generation {
		b.gen = generation
		b.count = 0
	}
	return b
}

func (s *podRecoveryBudgetStore) count(key string, generation int64) int {
	return s.load(key, generation).count
}

func (s *podRecoveryBudgetStore) increment(key string, generation int64) {
	s.load(key, generation).count++
}

func (s *podRecoveryBudgetStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.budgets, key)
}

func (r *BatchSandboxReconciler) podRecoveryStuckThreshold() time.Duration {
	if r.FeatureConfig != nil {
		return r.FeatureConfig.duration(featureConfigKeyPodRecoveryStuckThreshold, defaultPodRecoveryStuckThreshold)
	}
	return defaultPodRecoveryStuckThreshold
}

func (r *BatchSandboxReconciler) podRecoveryMaxAttempts() int {
	if r.FeatureConfig != nil {
		return r.FeatureConfig.positiveInt(featureConfigKeyPodRecoveryMaxAttempts, defaultPodRecoveryMaxAttempts)
	}
	return defaultPodRecoveryMaxAttempts
}

// podRecoveryClock returns the current time; overridable in tests.
func (r *BatchSandboxReconciler) podRecoveryClock() time.Time {
	if r.podRecoveryNow != nil {
		return r.podRecoveryNow()
	}
	return time.Now()
}

// recoverStuckPods replaces pods stuck in provisioning failures while the
// sandbox has never been Ready; the scale path recreates them by index.
func (r *BatchSandboxReconciler) recoverStuckPods(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) {
	key := controllerutils.GetControllerKey(batchSbx)
	if batchSbx.DeletionTimestamp != nil {
		podRecoveryBudgets.delete(key)
		return
	}
	// Later lifecycle phases (pause/resume, terminal freeze) own their own
	// failure semantics; drop the stale budget entry while at it.
	if batchSbx.Status.Phase != "" && batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhasePending {
		podRecoveryBudgets.delete(key)
		return
	}

	log := logf.FromContext(ctx)
	threshold := r.podRecoveryStuckThreshold()
	maxAttempts := r.podRecoveryMaxAttempts()
	now := r.podRecoveryClock()

	for _, pod := range pods {
		failure, stuck := detectProvisioningFailure(pod)
		if !stuck {
			continue
		}
		// A broken image fails identically on every node; replacement cannot
		// help, so surface it and keep the budget.
		if failure.Permanent {
			podRecoverySkips.WithLabelValues(failure.Condition, recoverySkipPermanentFailure).Inc()
			r.Recorder.Eventf(batchSbx, corev1.EventTypeWarning, eventReasonImagePullPermanentFailure,
				"Pod %s cannot pull image (%s: %s); this looks like an image-level problem, pod replacement skipped",
				pod.Name, failure.Reason, strings.TrimSpace(failure.Message))
			continue
		}
		// A never-Ready pod enters failure states within seconds of being
		// created, so the creation timestamp is a sufficient duration anchor.
		if now.Sub(pod.CreationTimestamp.Time) < threshold {
			continue
		}
		if podRecoveryBudgets.count(key, batchSbx.Generation) >= maxAttempts {
			podRecoverySkips.WithLabelValues(failure.Condition, recoverySkipBudgetExhausted).Inc()
			r.Recorder.Eventf(batchSbx, corev1.EventTypeWarning, eventReasonPodRecoveryLimitReached,
				"Pod %s stuck in %s; pod replacement limit (%d) reached", pod.Name, failure.Reason, maxAttempts)
			continue
		}
		// Skip graceful termination: on a broken node the kubelet may never
		// confirm the delete, blocking the same-name recreation.
		if err := r.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "Failed to delete stuck pod", "pod", pod.Name, "reason", failure.Reason)
			r.Recorder.Eventf(batchSbx, corev1.EventTypeWarning, eventReasonFailedDelete,
				"Failed to delete stuck pod %s: %v", pod.Name, err)
			continue
		}
		podRecoveryBudgets.increment(key, batchSbx.Generation)
		podRecoveryReplacements.WithLabelValues(failure.Condition).Inc()
		r.Recorder.Eventf(batchSbx, corev1.EventTypeNormal, eventReasonReplacedStuckPod,
			"Deleted pod %s stuck in %s; it will be recreated to retry on another node",
			pod.Name, failure.Reason)
	}
}
