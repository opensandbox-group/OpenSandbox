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
	gerrors "errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	poolassign "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/poolassign"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	taskscheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/requeueduration"
)

var (
	BatchSandboxScaleExpectations = expectations.NewScaleExpectations()
	DurationStore                 = requeueduration.DurationStore{}
)

const batchSandboxFirstPodIndex = 0

type taskScheduleResult struct {
	Running, Failed, Succeed, Unknown, Pending int32
}

// BatchSandboxReconciler reconciles a BatchSandbox object
type BatchSandboxReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	Recorder            record.EventRecorder
	ProfileStore        *poolassign.ProfileStore
	taskSchedulers      sync.Map
	StatusRVExpectation expectations.ResourceVersionExpectation
	// ResumePullSecret is the K8s Secret name for pulling snapshot images during resume.
	ResumePullSecret string
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BatchSandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *BatchSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := logf.FromContext(ctx)
	start := time.Now()
	var aggErrors []error
	defer func() {
		_ = DurationStore.Pop(req.String())
		log.Info("Reconcile finished", "duration", time.Since(start).String(), "requeueAfter", result.RequeueAfter.String(), "error", retErr)
	}()
	batchSbx := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, batchSbx); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// handle expire
	if expireAt := batchSbx.Spec.ExpireTime; expireAt != nil {
		now := time.Now()
		if expireAt.Time.Before(now) {
			if batchSbx.DeletionTimestamp == nil {
				log.Info("batch sandbox expired, delete", "expireAt", expireAt)
				if err := r.Delete(ctx, batchSbx); err != nil {
					if errors.IsNotFound(err) {
						return ctrl.Result{}, nil
					}
					return ctrl.Result{}, err
				}
			}
		} else {
			DurationStore.Push(types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String(), expireAt.Time.Sub(now))
		}
	}

	// task schedule
	taskStrategy := strategy.NewTaskSchedulingStrategy(batchSbx)

	// pool strategy
	poolStrategy := strategy.NewPoolStrategy(batchSbx)

	if profileName := poolStrategy.AssignProfile(); profileName != "" {
		updated, err := r.assignPool(ctx, batchSbx, profileName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to auto-assign pool: %w", err)
		}
		if updated {
			return ctrl.Result{}, nil
		}
	}

	// handle finalizers
	if batchSbx.DeletionTimestamp == nil {
		if taskStrategy.NeedTaskScheduling() {
			// Validate shardTaskPatches early so that a type-mismatch is surfaced
			// immediately as a status condition rather than causing a silent merge
			// failure deep in the reconcile loop.
			if patchErr := taskStrategy.ValidateShardTaskPatches(); patchErr != nil {
				log.Error(patchErr, "invalid shardTaskPatches detected, updating status condition")
				statusCopy := batchSbx.Status.DeepCopy()
				setConditionInStatus(statusCopy, sandboxv1alpha1.BatchSandboxConditionInvalidShardPatch,
					sandboxv1alpha1.ConditionTrue, "InvalidShardPatch", patchErr.Error())
				if statusErr := r.updateStatus(ctx, batchSbx, statusCopy); statusErr != nil {
					log.Error(statusErr, "failed to update status with InvalidShardPatch condition")
					return ctrl.Result{}, statusErr
				}
				// Return without error so the controller does not keep requeueing;
				// the resource must be corrected by the user.
				return ctrl.Result{}, nil
			}
			// Patches are valid — clear any stale InvalidShardPatch condition so users
			// know the fix was accepted.
			if hasCondition(batchSbx, sandboxv1alpha1.BatchSandboxConditionInvalidShardPatch) {
				statusCopy := batchSbx.Status.DeepCopy()
				setConditionInStatus(statusCopy, sandboxv1alpha1.BatchSandboxConditionInvalidShardPatch,
					sandboxv1alpha1.ConditionFalse, "", "")
				if statusErr := r.updateStatus(ctx, batchSbx, statusCopy); statusErr != nil {
					log.Error(statusErr, "failed to clear InvalidShardPatch condition")
					return ctrl.Result{}, statusErr
				}
			}
			if !controllerutil.ContainsFinalizer(batchSbx, FinalizerTaskCleanup) {
				err := utils.UpdateFinalizer(r.Client, batchSbx, utils.AddFinalizerOpType, FinalizerTaskCleanup)
				if err != nil {
					log.Error(err, "failed to add finalizer", "finalizer", FinalizerTaskCleanup)
				} else {
					log.Info("added finalizer", "finalizer", FinalizerTaskCleanup)
				}
				return ctrl.Result{}, err
			}
		}
	} else {
		if !taskStrategy.NeedTaskScheduling() {
			return ctrl.Result{}, nil
		}
		// If shardTaskPatches are invalid we cannot build task specs, but we must
		// not leave the resource stuck in Terminating. Clear the finalizer now and
		// record the error in status so the deletion can proceed.
		if patchErr := taskStrategy.ValidateShardTaskPatches(); patchErr != nil {
			log.Error(patchErr, "invalid shardTaskPatches on deletion path, clearing finalizer to unblock termination")
			statusCopy := batchSbx.Status.DeepCopy()
			setConditionInStatus(statusCopy, sandboxv1alpha1.BatchSandboxConditionInvalidShardPatch,
				sandboxv1alpha1.ConditionTrue, "InvalidShardPatch", patchErr.Error())
			if statusErr := r.updateStatus(ctx, batchSbx, statusCopy); statusErr != nil {
				log.Error(statusErr, "failed to update status with InvalidShardPatch condition on deletion path")
				return ctrl.Result{}, statusErr
			}
			// Always clean up the in-memory scheduler regardless of whether the finalizer
			// is still present, so stale schedulers are not left behind.
			r.deleteTaskScheduler(ctx, batchSbx)
			if controllerutil.ContainsFinalizer(batchSbx, FinalizerTaskCleanup) {
				if err := utils.UpdateFinalizer(r.Client, batchSbx, utils.RemoveFinalizerOpType, FinalizerTaskCleanup); err != nil {
					if !errors.IsNotFound(err) {
						log.Error(err, "failed to remove finalizer during invalid-patch deletion", "finalizer", FinalizerTaskCleanup)
						return ctrl.Result{}, err
					}
				}
				log.Info("removed finalizer due to invalid shardTaskPatches, resource can now be deleted")
			}
			return ctrl.Result{}, nil
		}
	}

	// Pause/Resume dispatch: handles pause/resume intent before normal scaling.
	if result, handled, err := r.dispatchPauseResume(ctx, batchSbx); handled {
		return result, err
	}

	// dispatchPauseResume may patch BatchSandbox spec/state (for example resume detaches a pooled
	// sandbox from its pool). Recompute strategies from the latest object before listing pods so
	// normal reconciliation does not keep using a stale pre-dispatch view.
	taskStrategy = strategy.NewTaskSchedulingStrategy(batchSbx)

	pods, err := r.listPods(ctx, poolStrategy, batchSbx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list pods %w", err)
	}
	podIndex, err := calPodIndex(poolStrategy, batchSbx, pods)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cal pod index %w", err)
	}
	slices.SortStableFunc(pods, utils.MultiPodSorter([]func(a, b *corev1.Pod) int{
		utils.WithPodIndexSorter(podIndex),
		utils.PodNameSorter,
	}).Sort)
	// Normal mode owns pod lifecycle except while a sandbox is fully paused. In Paused, the
	// snapshot-backed runtime is quiesced and pods must stay absent until resume rewrites the
	// template images and transitions back through Resuming.
	if !poolStrategy.IsPooledMode() && batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhasePaused {
		err := r.scaleBatchSandbox(ctx, batchSbx, batchSbx.Spec.Template, pods)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to scale batch sandbox %w", err)
		}
	}

	runtimeView := buildRuntimeView(batchSbx, pods)
	// Ensure PauseObservedGeneration is up-to-date so the status patch ACKs the
	// current generation without requiring a dedicated API call.
	// Skip during Resuming: a newer generation may carry a queued pause request
	// that must remain unacknowledged until resume completes and handlePause runs.
	if batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhaseResuming &&
		runtimeView.status.PauseObservedGeneration < batchSbx.Generation {
		runtimeView.status.PauseObservedGeneration = batchSbx.Generation
	}

	if batchSbx.Status.Phase == sandboxv1alpha1.BatchSandboxPhasePaused {
		r.deleteTaskScheduler(ctx, batchSbx)
	}

	if taskStrategy.NeedTaskScheduling() && batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhasePaused {
		ts, err := r.reconcileTasks(ctx, batchSbx, pods)
		if err != nil {
			aggErrors = append(aggErrors, err)
		} else if ts != nil {
			runtimeView.status.TaskRunning = ts.Running
			runtimeView.status.TaskFailed = ts.Failed
			runtimeView.status.TaskSucceed = ts.Succeed
			runtimeView.status.TaskUnknown = ts.Unknown
			runtimeView.status.TaskPending = ts.Pending
		}
	}

	requeue, persistErrors := r.persistRuntimeView(ctx, batchSbx, runtimeView)
	aggErrors = append(aggErrors, persistErrors...)

	requeueAfter := DurationStore.Pop(req.String())
	if requeue > 0 && (requeueAfter == 0 || requeue < requeueAfter) {
		requeueAfter = requeue
	}
	return reconcile.Result{RequeueAfter: requeueAfter}, gerrors.Join(aggErrors...)
}

func calPodIndex(poolStrategy strategy.PoolStrategy, batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (map[string]int, error) {
	podIndex := map[string]int{}
	if poolStrategy.IsPooledMode() {
		// cal index from pool alloc result while using pooling
		alloc, err := parseSandboxAllocation(batchSbx)
		if err != nil {
			return nil, err
		}
		for i := range alloc.Pods {
			podIndex[alloc.Pods[i]] = i
		}
	} else {
		for i := range pods {
			po := pods[i]
			idx, err := parseIndex(po)
			if err != nil {
				return nil, fmt.Errorf("batchsandbox: failed to parse %s/%s index %w", po.Namespace, po.Name, err)
			}
			podIndex[po.Name] = idx
		}
	}
	return podIndex, nil
}

func (r *BatchSandboxReconciler) reconcileTasks(
	ctx context.Context,
	batchSbx *sandboxv1alpha1.BatchSandbox,
	pods []*corev1.Pod,
) (*taskScheduleResult, error) {
	log := logf.FromContext(ctx)

	sch, err := r.getTaskScheduler(ctx, batchSbx, pods)
	if err != nil {
		return nil, err
	}

	// Because tasks are in-memory and there is no event mechanism, periodic reconciliation is required.
	DurationStore.Push(types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String(), 3*time.Second)

	if batchSbx.DeletionTimestamp != nil {
		stoppingTasks := sch.StopTask()
		if len(stoppingTasks) > 0 {
			log.Info("stopping tasks", "count", len(stoppingTasks))
		}
	}

	now := time.Now()
	ts, err := r.scheduleTasks(ctx, sch, batchSbx)
	if err != nil {
		return nil, fmt.Errorf("failed to schedule tasks, err %w", err)
	}
	log.Info("schedule tasks completed", "costMs", time.Since(now).Milliseconds(), "task schedule result", utils.DumpJSON(ts))

	// check task cleanup is finished
	if batchSbx.DeletionTimestamp != nil {
		unfinishedTasks := r.getTasksCleanupUnfinished(batchSbx, sch)
		if len(unfinishedTasks) > 0 {
			log.Info("tasks cleanup is unfinished", "unfinishedCount", len(unfinishedTasks))
		} else {
			var cleanupErr error
			if controllerutil.ContainsFinalizer(batchSbx, FinalizerTaskCleanup) {
				cleanupErr = utils.UpdateFinalizer(r.Client, batchSbx, utils.RemoveFinalizerOpType, FinalizerTaskCleanup)
				if cleanupErr != nil {
					if errors.IsNotFound(cleanupErr) {
						cleanupErr = nil
					} else {
						log.Error(cleanupErr, "failed to remove finalizer", "finalizer", FinalizerTaskCleanup)
					}
				}
			}
			if cleanupErr == nil {
				r.deleteTaskScheduler(ctx, batchSbx)
				log.Info("task cleanup is finished, removed finalizer", "finalizer", FinalizerTaskCleanup)
			}
			// all tasks are cleaned up; skip returning task schedule result so the caller doesn't overwrite status
			return nil, cleanupErr
		}
	}

	return ts, nil
}

func (r *BatchSandboxReconciler) listPods(ctx context.Context, poolStrategy strategy.PoolStrategy, batchSbx *sandboxv1alpha1.BatchSandbox) ([]*corev1.Pod, error) {
	var ret []*corev1.Pod
	if poolStrategy.IsPooledMode() {
		var (
			allocSet    = make(sets.Set[string])
			releasedSet = make(sets.Set[string])
		)
		alloc, err := parseSandboxAllocation(batchSbx)
		if err != nil {
			return nil, err
		}
		allocSet.Insert(alloc.Pods...)

		released, err := parseSandboxReleased(batchSbx)
		if err != nil {
			return nil, err
		}
		releasedSet.Insert(released.Pods...)

		activePods := allocSet.Difference(releasedSet)
		for name := range activePods {
			pod := &corev1.Pod{}
			// TODO maybe performance is problem
			if err := r.Client.Get(ctx, types.NamespacedName{Namespace: batchSbx.Namespace, Name: name}, pod); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			ret = append(ret, pod)
		}
	} else {
		podList := &corev1.PodList{}
		if err := r.Client.List(ctx, podList, &client.ListOptions{
			Namespace:     batchSbx.Namespace,
			FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(batchSbx.UID)}),
		}); err != nil {
			return nil, err
		}
		for i := range podList.Items {
			ret = append(ret, &podList.Items[i])
		}
	}
	return ret, nil
}

func (r *BatchSandboxReconciler) getTaskScheduler(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (taskscheduler.TaskScheduler, error) {
	log := logf.FromContext(ctx)
	var tSch taskscheduler.TaskScheduler
	key := types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String()
	val, ok := r.taskSchedulers.Load(key)
	// The reconciler guarantees that it will not concurrently reconcile the same BatchSandbox.
	if !ok {
		policy := sandboxv1alpha1.TaskResourcePolicyRetain
		if batchSbx.Spec.TaskResourcePolicyWhenCompleted != nil {
			policy = *batchSbx.Spec.TaskResourcePolicyWhenCompleted
		}
		taskStrategy := strategy.NewTaskSchedulingStrategy(batchSbx)
		taskSpecs, err := taskStrategy.GenerateTaskSpecs()
		if err != nil {
			return nil, err
		}
		sc, err := taskscheduler.NewTaskScheduler(key, taskSpecs, pods, policy, log)
		if err != nil {
			return nil, fmt.Errorf("new task scheduler err %w", err)
		}
		log.Info("successfully created task scheduler")
		tSch = sc
		r.taskSchedulers.Store(key, sc)
	} else {
		tSch, ok = (val.(taskscheduler.TaskScheduler))
		if !ok {
			return nil, gerrors.New("invalid scheduler type stored")
		}
		// Update the pods list for this scheduler
		tSch.UpdatePods(pods)
		// Handle scale-out: register task specs for any replicas added since the
		// scheduler was first created. Already-tracked task names are skipped.
		taskStrategy := strategy.NewTaskSchedulingStrategy(batchSbx)
		taskSpecs, err := taskStrategy.GenerateTaskSpecs()
		if err != nil {
			return nil, fmt.Errorf("failed to generate task specs for scale-out: %w", err)
		}
		if err := tSch.AddTasks(taskSpecs); err != nil {
			return nil, fmt.Errorf("failed to add tasks on scale-out: %w", err)
		}
	}
	return tSch, nil
}

func (r *BatchSandboxReconciler) deleteTaskScheduler(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox) {
	key := types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String()
	if _, ok := r.taskSchedulers.LoadAndDelete(key); ok {
		log := logf.FromContext(ctx)
		log.Info("delete task scheduler")
	}
}

func (r *BatchSandboxReconciler) scheduleTasks(ctx context.Context, tSch taskscheduler.TaskScheduler, batchSbx *sandboxv1alpha1.BatchSandbox) (*taskScheduleResult, error) {
	log := logf.FromContext(ctx)
	if err := tSch.Schedule(); err != nil {
		return nil, err
	}
	tasks := tSch.ListTask()
	toReleasedPods := []string{}
	var (
		running, failed, succeed, unknown int32
		pending                           int32
	)
	for i := range len(tasks) {
		task := tasks[i]
		if task.GetPodName() == "" {
			pending++
		} else {
			state := task.GetState()
			if task.IsResourceReleased() {
				toReleasedPods = append(toReleasedPods, task.GetPodName())
			}
			switch state {
			case taskscheduler.RunningTaskState:
				running++
			case taskscheduler.SucceedTaskState:
				succeed++
			case taskscheduler.FailedTaskState:
				failed++
			case taskscheduler.UnknownTaskState:
				unknown++
			}
		}
	}
	if len(toReleasedPods) > 0 {
		log.Info("try to release Pods", "count", len(toReleasedPods))
		if err := r.releasePods(ctx, batchSbx, toReleasedPods); err != nil {
			return nil, err
		}
		log.Info("successfully released Pods", "count", len(toReleasedPods))
	}
	return &taskScheduleResult{
		Running: running,
		Failed:  failed,
		Succeed: succeed,
		Unknown: unknown,
		Pending: pending,
	}, nil
}

func (r *BatchSandboxReconciler) getTasksCleanupUnfinished(batchSbx *sandboxv1alpha1.BatchSandbox, tSch taskscheduler.TaskScheduler) []taskscheduler.Task {
	var notReleased []taskscheduler.Task
	for _, task := range tSch.ListTask() {
		if !task.IsResourceReleased() {
			notReleased = append(notReleased, task)
		}
	}
	return notReleased
}

func (r *BatchSandboxReconciler) releasePods(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, toReleasePods []string) error {
	releasedSet := make(sets.Set[string])
	released, err := parseSandboxReleased(batchSbx)
	if err != nil {
		return err
	}
	releasedSet.Insert(released.Pods...)
	releasedSet.Insert(toReleasePods...)
	newRelease := AllocationRelease{
		Pods: sets.List(releasedSet),
	}
	raw, err := json.Marshal(newRelease)
	if err != nil {
		return fmt.Errorf("Failed to marshal released pod names: %v", err)
	}
	body := utils.DumpJSON(struct {
		MetaData metav1.ObjectMeta `json:"metadata"`
	}{
		MetaData: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnoAllocReleaseKey: string(raw),
			},
		},
	})
	b := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: batchSbx.Namespace,
			Name:      batchSbx.Name,
		},
	}
	return r.Client.Patch(ctx, b, client.RawPatch(types.MergePatchType, []byte(body)))
}

// Normal Mode
func (r *BatchSandboxReconciler) scaleBatchSandbox(ctx context.Context, batchSandbox *sandboxv1alpha1.BatchSandbox, podTemplateSpec *corev1.PodTemplateSpec, pods []*corev1.Pod) error {
	log := logf.FromContext(ctx)
	indexedPodMap := map[int]*corev1.Pod{}
	for i := range pods {
		pod := pods[i]
		BatchSandboxScaleExpectations.ObserveScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
		idx, err := parseIndex(pod)
		if err != nil {
			return fmt.Errorf("failed to parse idx Pod %s, err %w", pod.Name, err)
		}
		indexedPodMap[idx] = pod
	}
	if satisfied, unsatisfiedDuration, dirtyPods := BatchSandboxScaleExpectations.SatisfiedExpectations(controllerutils.GetControllerKey(batchSandbox)); !satisfied {
		log.Info("scale expectation is not satisfied", "unsatisfiedDuration", unsatisfiedDuration, "dirtyPods", dirtyPods)
		DurationStore.Push(types.NamespacedName{Namespace: batchSandbox.Namespace, Name: batchSandbox.Name}.String(), expectations.ExpectationTimeout-unsatisfiedDuration)
		return nil
	}
	// TODO consider supply Pods if Pods is deleted unexpectedly
	var needCreateIndex []int
	// TODO var needDeleteIndex []int
	for i := 0; i < int(*batchSandbox.Spec.Replicas); i++ {
		_, ok := indexedPodMap[i]
		if !ok {
			needCreateIndex = append(needCreateIndex, i)
		}
	}
	// scale
	if len(needCreateIndex) > 0 {
		log.Info("try to create Pods", "count", len(needCreateIndex), "indexes", needCreateIndex)
	}
	for _, idx := range needCreateIndex {
		pod, err := utils.GetPodFromTemplate(podTemplateSpec, batchSandbox, metav1.NewControllerRef(batchSandbox, sandboxv1alpha1.SchemeBuilder.GroupVersion.WithKind("BatchSandbox")))
		if err != nil {
			return err
		}
		// Apply shard patch if available for this index
		if len(batchSandbox.Spec.ShardPatches) > 0 && idx < len(batchSandbox.Spec.ShardPatches) {
			podBytes, err := json.Marshal(pod)
			if err != nil {
				return fmt.Errorf("failed to marshal pod: %w", err)
			}
			patch := batchSandbox.Spec.ShardPatches[idx]
			modifiedPodBytes, err := strategicpatch.StrategicMergePatch(podBytes, patch.Raw, &corev1.Pod{})
			if err != nil {
				return fmt.Errorf("failed to apply shard patch for index %d: %w", idx, err)
			}
			if err := json.Unmarshal(modifiedPodBytes, pod); err != nil {
				return fmt.Errorf("failed to unmarshal patched pod for index %d: %w", idx, err)
			}
		}
		if err := ctrl.SetControllerReference(pod, batchSandbox, r.Scheme); err != nil {
			return err
		}
		pod.Labels[LabelBatchSandboxPodIndexKey] = strconv.Itoa(idx)
		pod.Labels[LabelBatchSandboxNameKey] = batchSandbox.Name
		pod.Namespace = batchSandbox.Namespace
		pod.Name = fmt.Sprintf("%s-%d", batchSandbox.Name, idx)
		BatchSandboxScaleExpectations.ExpectScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
		if err := r.Create(ctx, pod); err != nil {
			BatchSandboxScaleExpectations.ObserveScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
			r.Recorder.Eventf(batchSandbox, corev1.EventTypeWarning, "FailedCreate", "failed to create pod: %v, pod: %v", err, utils.DumpJSON(pod))
			return err
		}
		r.Recorder.Eventf(batchSandbox, corev1.EventTypeNormal, "SuccessfulCreate", "succeed to create pod %s", pod.Name)
	}
	return nil
}

func parseIndex(pod *corev1.Pod) (int, error) {
	if v := pod.Labels[LabelBatchSandboxPodIndexKey]; v != "" {
		return strconv.Atoi(v)
	}
	idx := strings.LastIndex(pod.Name, "-")
	if idx == -1 {
		return -1, gerrors.New("batchsandbox: Invalid pod Name")
	}
	return strconv.Atoi(pod.Name[idx+1:])
}

// assignPool selects a Pool for the BatchSandbox using the assign package and writes
// the result back to spec.poolRef. Returns (true, nil) when the update was applied,
// which triggers a new reconcile with the concrete poolRef.
func (r *BatchSandboxReconciler) assignPool(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, profileName string) (bool, error) {
	log := logf.FromContext(ctx)

	poolList := &sandboxv1alpha1.PoolList{}
	if err := r.List(ctx, poolList, client.InNamespace(batchSbx.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list pools: %w", err)
	}

	pools := make([]*sandboxv1alpha1.Pool, 0, len(poolList.Items))
	for i := range poolList.Items {
		pools = append(pools, &poolList.Items[i])
	}

	profile := r.ProfileStore.GetProfile(profileName)
	assigner := poolassign.NewDefaultAssigner(profile)

	poolName, err := assigner.AssignPool(ctx, batchSbx, pools)
	if err != nil {
		return false, err
	}

	oldSbx := batchSbx.DeepCopy()
	batchSbx.Spec.PoolRef = poolName
	patch := client.MergeFrom(oldSbx)
	if err := r.Patch(ctx, batchSbx, patch); err != nil {
		return false, fmt.Errorf("failed to patch poolRef: %w", err)
	}

	log.Info("auto-assigned pool", "pool", poolName)
	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BatchSandboxReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconciles int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.BatchSandbox{}).
		Named("batchsandbox").
		Owns(&corev1.Pod{}).
		Owns(&sandboxv1alpha1.SandboxSnapshot{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}
