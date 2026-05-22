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
	"crypto/sha256"
	"encoding/hex"
	gerrors "errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/eviction"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/recycle"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

const (
	defaultRetryTime = 5 * time.Second
)

const (
	LabelPoolName     = "sandbox.opensandbox.io/pool-name"
	LabelPoolRevision = "sandbox.opensandbox.io/pool-revision"
)

const (
	defaultSyncSandboxAllocConcurrency = 256
	envSyncSandboxAllocConcurrency     = "SYNC_SANDBOX_ALLOC_CONCURRENCY"

	defaultRecyclePodConcurrency = 64
	envRecyclePodConcurrency     = "RECYCLE_POD_CONCURRENCY"
)

var (
	PoolScaleExpectations       = expectations.NewScaleExpectations()
	syncSandboxAllocConcurrency int
	recyclePodConcurrency       int
)

func init() {
	syncSandboxAllocConcurrency = defaultSyncSandboxAllocConcurrency
	if val := os.Getenv(envSyncSandboxAllocConcurrency); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			syncSandboxAllocConcurrency = n
		}
	}
	recyclePodConcurrency = defaultRecyclePodConcurrency
	if val := os.Getenv(envRecyclePodConcurrency); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			recyclePodConcurrency = n
		}
	}
}

// PoolReconciler reconciles a Pool object
type PoolReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	Allocator  Allocator
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=pools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=pools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=pools/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := logf.FromContext(ctx)
	start := time.Now()
	defer func() {
		log.Info("Reconcile finished", "duration", time.Since(start).String(), "requeueAfter", result.RequeueAfter.String(), "error", retErr)
	}()
	// Fetch the Pool instance
	pool := &sandboxv1alpha1.Pool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		if errors.IsNotFound(err) {
			// Pool resource not found, could have been deleted
			controllerKey := req.NamespacedName.String()
			PoolScaleExpectations.DeleteExpectations(controllerKey)
			r.Allocator.ClearPoolAllocation(ctx, req.Namespace, req.Name)
			log.Info("Pool resource not found, cleaned up scale expectations", "pool", controllerKey)
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request
		log.Error(err, "Failed to get Pool")
		return ctrl.Result{}, err
	}
	if !pool.DeletionTimestamp.IsZero() {
		controllerKey := controllerutils.GetControllerKey(pool)
		PoolScaleExpectations.DeleteExpectations(controllerKey)
		r.Allocator.ClearPoolAllocation(ctx, req.Namespace, req.Name)
		log.Info("Pool resource is being deleted, cleaned up scale expectations", "pool", controllerKey)
		return ctrl.Result{}, nil
	}

	// List all pods of the pool
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, &client.ListOptions{
		Namespace:     pool.Namespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
	}); err != nil {
		log.Error(err, "Failed to list pods")
		return reconcile.Result{}, err
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := podList.Items[i]
		PoolScaleExpectations.ObserveScale(controllerutils.GetControllerKey(pool), expectations.Create, pod.Name)
		if pod.DeletionTimestamp.IsZero() {
			pods = append(pods, &pod)
		}
	}

	// List all batch sandboxes  ref to the pool
	batchSandboxList := &sandboxv1alpha1.BatchSandboxList{}
	if err := r.List(ctx, batchSandboxList, &client.ListOptions{
		Namespace:     pool.Namespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForPoolRef: pool.Name}),
	}); err != nil {
		log.Error(err, "Failed to list batch sandboxes")
		return reconcile.Result{}, err
	}
	batchSandboxes := make([]*sandboxv1alpha1.BatchSandbox, 0, len(batchSandboxList.Items))
	for i := range batchSandboxList.Items {
		batchSandbox := batchSandboxList.Items[i]
		if batchSandbox.Spec.Template != nil {
			continue
		}
		batchSandboxes = append(batchSandboxes, &batchSandbox)
	}
	log.Info("Pool reconcile", "pool", pool.Name, "pods", len(pods), "batchSandboxes", len(batchSandboxes))
	return r.reconcilePool(ctx, pool, batchSandboxes, pods)
}

// reconcilePool contains the main reconciliation logic
func (r *PoolReconciler) reconcilePool(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (ctrl.Result, error) {
	var result ctrl.Result

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// 1. Get latest Pool CR
		latestPool := &sandboxv1alpha1.Pool{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), latestPool); err != nil {
			return err
		}

		// 2. Handle pod eviction
		schedulePods, evictionErr := r.handleEviction(ctx, latestPool, pods)
		if schedulePods == nil {
			return evictionErr
		}

		// 3. Schedule sandbox (compute + persist + sync)
		schedResult, err := r.scheduleSandbox(ctx, latestPool, batchSandboxes, schedulePods)
		if err != nil {
			return err
		}
		// Requeue if there are pending sandboxes waiting for scheduling
		if schedResult.SupplyCnt > 0 {
			result = ctrl.Result{RequeueAfter: defaultRetryTime}
		}

		// 4. Handle pool upgrade
		updateResult, err := r.updatePool(ctx, latestPool, schedulePods, schedResult.IdlePods)
		if err != nil {
			return err
		}

		// 5. Handle pool scale
		toDeletePods := append(updateResult.ToDeletePods, schedResult.ToDelete...)
		args := &scaleArgs{
			updateRevision: updateResult.UpdateRevision,
			pods:           schedulePods,
			totalPodCnt:    int32(len(pods)),
			allocatedCnt:   int32(len(schedResult.LatestAllocation)),
			idlePods:       updateResult.IdlePods,
			toDeletePods:   toDeletePods,
			supplyCnt:      schedResult.SupplyCnt + updateResult.SupplyUpdateRevision,
		}

		if err := r.scalePool(ctx, latestPool, args); err != nil {
			return err
		}

		// 6. Update pool status
		if err := r.updatePoolStatus(ctx, updateResult.UpdateRevision, latestPool, pods, schedulePods, schedResult.LatestAllocation); err != nil {
			return err
		}

		if evictionErr != nil {
			return evictionErr
		}

		return nil
	})

	return result, err
}

func (r *PoolReconciler) calculateRevision(pool *sandboxv1alpha1.Pool) (string, error) {
	template, err := json.Marshal(pool.Spec.Template)
	if err != nil {
		return "", err
	}
	revision := sha256.Sum256(template)
	return hex.EncodeToString(revision[:8]), nil
}

// SetupWithManager sets up the controller with the Manager.
// Todo pod deletion expectations
func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconciles int) error {
	filterBatchSandbox := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			bsb, ok := e.Object.(*sandboxv1alpha1.BatchSandbox)
			if !ok {
				return false
			}
			return bsb.Spec.PoolRef != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObj, okOld := e.ObjectOld.(*sandboxv1alpha1.BatchSandbox)
			newObj, okNew := e.ObjectNew.(*sandboxv1alpha1.BatchSandbox)
			if !okOld || !okNew {
				return false
			}
			if newObj.Spec.PoolRef == "" {
				return false
			}
			oldVal := oldObj.Annotations[AnnoAllocReleaseKey]
			newVal := newObj.Annotations[AnnoAllocReleaseKey]
			if oldVal != newVal {
				return true
			}
			if oldObj.Spec.Replicas != newObj.Spec.Replicas {
				return true
			}
			// Trigger reconcile when sandbox enters terminating state (DeletionTimestamp is set).
			if oldObj.DeletionTimestamp.IsZero() && !newObj.DeletionTimestamp.IsZero() {
				return true
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			bsb, ok := e.Object.(*sandboxv1alpha1.BatchSandbox)
			if !ok {
				return false
			}
			return bsb.Spec.PoolRef != ""
		},
		GenericFunc: func(e event.GenericEvent) bool {
			bsb, ok := e.Object.(*sandboxv1alpha1.BatchSandbox)
			if !ok {
				return false
			}
			return bsb.Spec.PoolRef != ""
		},
	}

	findPoolForBatchSandbox := func(ctx context.Context, obj client.Object) []reconcile.Request {
		log := logf.FromContext(ctx)
		batchSandbox, ok := obj.(*sandboxv1alpha1.BatchSandbox)
		if !ok {
			log.Error(nil, "Invalid object type, expected BatchSandbox")
			return nil
		}
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: batchSandbox.Namespace,
					Name:      batchSandbox.Spec.PoolRef,
				},
			},
		}
	}

	filterBatchSandboxDetached := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObj, okOld := e.ObjectOld.(*sandboxv1alpha1.BatchSandbox)
			newObj, okNew := e.ObjectNew.(*sandboxv1alpha1.BatchSandbox)
			if !okOld || !okNew {
				return false
			}
			return oldObj.Spec.PoolRef != "" && newObj.Spec.PoolRef == ""
		},
	}

	enqueueOldPoolForDetachedBatchSandbox := handler.Funcs{
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldObj, ok := e.ObjectOld.(*sandboxv1alpha1.BatchSandbox)
			if !ok || oldObj.Spec.PoolRef == "" {
				return
			}
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: oldObj.Namespace,
					Name:      oldObj.Spec.PoolRef,
				},
			})
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.Pool{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Watches(
			&sandboxv1alpha1.BatchSandbox{},
			handler.EnqueueRequestsFromMapFunc(findPoolForBatchSandbox),
			builder.WithPredicates(filterBatchSandbox),
		).
		Watches(
			&sandboxv1alpha1.BatchSandbox{},
			enqueueOldPoolForDetachedBatchSandbox,
			builder.WithPredicates(filterBatchSandboxDetached),
		).
		Named("pool").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

func (r *PoolReconciler) doAllocate(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod, toAllocate map[string][]string) error {
	// 1. Compute latest allocated pods per sandbox (merge current + newly allocated).
	toSyncMap := r.getLatestAllocated(ctx, pool, batchSandboxes, toAllocate)

	// 2. Concurrently sync each sandbox's Allocated annotation (AddFinalizer is called inside SyncSandboxAllocation).
	return r.syncSandboxConcurrently(ctx, batchSandboxes, toSyncMap, r.Allocator.SyncSandboxAllocation, "allocated")
}

// getLatestAllocated computes the latest allocated pods for each sandbox by merging current allocation with new pods to allocate.
func (r *PoolReconciler) getLatestAllocated(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, toAllocate map[string][]string) map[string][]string {
	log := logf.FromContext(ctx)
	sandboxByName := make(map[string]*sandboxv1alpha1.BatchSandbox, len(batchSandboxes))
	for _, bs := range batchSandboxes {
		sandboxByName[bs.Name] = bs
	}

	toSyncMap := make(map[string][]string, len(toAllocate))
	for sandboxName, allocPods := range toAllocate {
		if len(allocPods) == 0 {
			continue
		}
		sandbox, ok := sandboxByName[sandboxName]
		if !ok {
			log.Error(nil, "Sandbox not found for allocate", "sandbox", sandboxName)
			continue
		}
		currentAllocated, err := r.Allocator.GetSandboxAllocation(ctx, sandbox)
		if err != nil {
			log.Error(err, "Failed to get sandbox allocated", "sandbox", sandboxName)
			continue
		}
		toSyncMap[sandboxName] = append(currentAllocated, allocPods...)
	}
	return toSyncMap
}

// syncSandboxConcurrently syncs allocation or released state for each sandbox concurrently.
// Each sandbox is an independent resource, so concurrent writes are safe.
func (r *PoolReconciler) syncSandboxConcurrently(ctx context.Context, batchSandboxes []*sandboxv1alpha1.BatchSandbox, toSyncMap map[string][]string, syncFn func(context.Context, *sandboxv1alpha1.BatchSandbox, []string) error, label string) error {
	log := logf.FromContext(ctx)

	sandboxByName := make(map[string]*sandboxv1alpha1.BatchSandbox, len(batchSandboxes))
	for _, bs := range batchSandboxes {
		sandboxByName[bs.Name] = bs
	}

	errCh := make(chan error, len(toSyncMap))
	sem := make(chan struct{}, syncSandboxAllocConcurrency)
	var wg sync.WaitGroup
	for sandboxName, pods := range toSyncMap {
		sandbox, ok := sandboxByName[sandboxName]
		if !ok {
			log.Error(nil, "Sandbox not found for sync "+label, "sandbox", sandboxName)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := syncFn(ctx, sandbox, pods); err != nil {
				log.Error(err, "Failed to sync sandbox "+label, "sandbox", sandbox.Name)
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return gerrors.Join(errs...)
}

func (r *PoolReconciler) doRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod, toRecycle map[string][]string) (map[string][]string, []string, error) {
	if len(toRecycle) == 0 {
		return nil, nil, nil
	}

	handler, err := recycle.NewHandler(r.Client, r.RestConfig, pool)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get recycle handler for pool %s: %w", pool.Name, err)
	}

	results := r.runRecycleTasks(ctx, pool, pods, toRecycle, handler)
	return collectRecycleResults(ctx, results)
}

type recycleResult struct {
	sandboxName string
	podName     string
	status      *recycle.Status
	err         error
}

// runRecycleTasks executes TryRecycle concurrently for each (sandbox, pod) pair and
// returns one result per task.
func (r *PoolReconciler) runRecycleTasks(ctx context.Context, pool *sandboxv1alpha1.Pool, pods []*corev1.Pod, toRecycle map[string][]string, handler recycle.Handler) []recycleResult {
	podByName := make(map[string]*corev1.Pod, len(pods))
	for _, p := range pods {
		podByName[p.Name] = p
	}

	// Flatten the map into an ordered slice so goroutines can write by index.
	type task struct {
		sandboxName string
		podName     string
	}
	var tasks []task
	for sandboxName, podNames := range toRecycle {
		for _, podName := range podNames {
			tasks = append(tasks, task{sandboxName: sandboxName, podName: podName})
		}
	}

	// Results are written by index so each goroutine writes to a unique slot without synchronization.
	results := make([]recycleResult, len(tasks))
	sem := make(chan struct{}, recyclePodConcurrency)
	var wg sync.WaitGroup
	for idx, task := range tasks {
		localIdx, localTask := idx, task
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, err := handler.TryRecycle(ctx, pool, podByName[localTask.podName], &recycle.Spec{ID: localTask.sandboxName})
			results[localIdx] = recycleResult{sandboxName: localTask.sandboxName, podName: localTask.podName, status: status, err: err}
		}()
	}
	wg.Wait()
	return results
}

// collectRecycleResults aggregates per-task results into the succeed map and delete list.
func collectRecycleResults(ctx context.Context, results []recycleResult) (map[string][]string, []string, error) {
	log := logf.FromContext(ctx)
	succeedMap := make(map[string][]string)
	var toDeletePods []string
	var errs []error

	for _, res := range results {
		if res.err != nil {
			log.Error(res.err, "Failed to recycle pod", "pod", res.podName, "sandbox", res.sandboxName)
			errs = append(errs, res.err)
			continue
		}
		if res.status.State == recycle.StateSucceeded {
			succeedMap[res.sandboxName] = append(succeedMap[res.sandboxName], res.podName)
		}
		if res.status.NeedDelete {
			toDeletePods = append(toDeletePods, res.podName)
		}
	}
	return succeedMap, toDeletePods, gerrors.Join(errs...)
}

// doRelease runs the recycle operation for pods to be returned to the pool,
// then persists the released state to each sandbox's annotation.
func (r *PoolReconciler) doRelease(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod, toRelease map[string][]string) ([]string, error) {
	log := logf.FromContext(ctx)

	// 1. Recycle pods.
	succeedMap, toDeletePods, err := r.doRecycle(ctx, pool, batchSandboxes, pods, toRelease)
	if err != nil {
		log.Error(err, "Some errors occurred during recycle")
	}

	// 2. Compute latest released pods per sandbox (merge current + recycle-succeeded).
	// Also collect orphan pods whose sandboxes no longer exist.
	toSyncMap, orphanPods := r.getLatestReleased(ctx, batchSandboxes, succeedMap)

	// 3. Concurrently sync each sandbox's Released annotation.
	syncErr := r.syncSandboxConcurrently(ctx, batchSandboxes, toSyncMap, r.Allocator.SyncSandboxReleased, "released")
	if syncErr != nil {
		log.Error(syncErr, "Failed to sync released")
	}

	// 4. Release in-memory allocations for orphan pods directly (no annotation to persist).
	if len(orphanPods) > 0 {
		r.Allocator.ReleasePodsAllocation(ctx, pool.Namespace, pool.Name, orphanPods)
	}

	return toDeletePods, gerrors.Join(err, syncErr)
}

// getLatestReleased computes the latest released pods for each sandbox by merging current released with recycle-succeeded pods.
// It also returns orphanPods: pods from succeedMap whose sandbox no longer exists and should be released directly by the caller.
func (r *PoolReconciler) getLatestReleased(ctx context.Context, batchSandboxes []*sandboxv1alpha1.BatchSandbox, succeedMap map[string][]string) (map[string][]string, []string) {
	log := logf.FromContext(ctx)
	sandboxByName := make(map[string]*sandboxv1alpha1.BatchSandbox, len(batchSandboxes))
	for _, bs := range batchSandboxes {
		sandboxByName[bs.Name] = bs
	}

	toSyncMap := make(map[string][]string, len(succeedMap))
	orphanPods := make([]string, 0)
	for sandboxName, succeedPods := range succeedMap {
		if len(succeedPods) == 0 {
			continue
		}
		sandbox, ok := sandboxByName[sandboxName]
		if !ok {
			// Orphan sandbox: deleted before recycle completed. Collect its pods for direct release.
			log.Info("GC: sandbox not found for recycle result, collecting orphan pods", "sandbox", sandboxName, "pods", succeedPods)
			orphanPods = append(orphanPods, succeedPods...)
			continue
		}
		currentReleased, err := r.Allocator.GetSandboxReleased(ctx, sandbox)
		if err != nil {
			log.Error(err, "Failed to get sandbox released", "sandbox", sandboxName)
			continue
		}
		toSyncMap[sandboxName] = append(currentReleased, succeedPods...)
	}
	return toSyncMap, orphanPods
}

func (r *PoolReconciler) scheduleSandbox(ctx context.Context, pool *sandboxv1alpha1.Pool, batchSandboxes []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (*ScheduleResult, error) {
	log := logf.FromContext(ctx)
	// 1. Compute scheduling actions.
	spec := &AllocSpec{
		Sandboxes: batchSandboxes,
		Pool:      pool,
		Pods:      pods,
	}
	allocAction, err := r.Allocator.Schedule(ctx, spec)
	if err != nil {
		return nil, err
	}
	log.Info("Allocate action", "pool", pool.Name, "toAllocate", allocAction.ToAllocate, "toRelease", allocAction.ToRelease)

	// 2. Execute scheduling actions.
	// 2.1 Execute ToAllocate / update in-memory store.
	err = r.doAllocate(ctx, pool, batchSandboxes, pods, allocAction.ToAllocate)
	if err != nil {
		return nil, err
	}
	// 2.2 Execute ToRelease / release in-memory store.
	toDeletePods, err := r.doRelease(ctx, pool, batchSandboxes, pods, allocAction.ToRelease)
	if err != nil {
		return nil, err
	}

	// 3. Return schedule result
	latestAllocation, err := r.Allocator.GetPoolAllocation(ctx, pool)
	if err != nil {
		return nil, err
	}
	idlePods := make([]string, 0)
	for _, pod := range pods {
		if _, ok := latestAllocation[pod.Name]; !ok {
			idlePods = append(idlePods, pod.Name)
		}
	}
	result := &ScheduleResult{
		LatestAllocation: latestAllocation,
		IdlePods:         idlePods,
		ToDelete:         toDeletePods,
		SupplyCnt:        allocAction.PodSupplement,
	}
	log.Info("Schedule result", "pool", pool.Name, "toDeletePods", toDeletePods, "supplyCnt", allocAction.PodSupplement)
	return result, nil
}

func (r *PoolReconciler) updatePool(ctx context.Context, pool *sandboxv1alpha1.Pool, pods []*corev1.Pod, idlePods []string) (*UpdateResult, error) {
	updateRevision, err := r.calculateRevision(pool)
	if err != nil {
		return nil, err
	}
	strategy := NewPoolUpdateStrategy(pool)
	result := strategy.Compute(ctx, updateRevision, pods, idlePods)
	result.UpdateRevision = updateRevision
	return result, nil
}

type scaleArgs struct {
	updateRevision string
	pods           []*corev1.Pod
	totalPodCnt    int32 // all pods including evicting ones, for PoolMax enforcement
	allocatedCnt   int32
	supplyCnt      int32 // to create
	idlePods       []string
	toDeletePods   []string
}

type ScheduleResult struct {
	// LatestAllocation is the most recent pod-to-sandbox allocation map.
	LatestAllocation map[string]string
	// IdlePods contains pods that are not currently allocated to any sandbox.
	IdlePods []string
	// ToDelete contains pods that the recycle handler has decided to delete
	// (e.g. direct deletion or restart failure fallback).
	ToDelete []string
	// SupplyCnt is the number of additional pods the allocator needs but are not yet available.
	SupplyCnt int32
}

type UpdateResult struct {
	UpdateRevision string
	IdlePods       []string
	ToDeletePods   []string
	// Supply Pods with update revision
	SupplyUpdateRevision int32
}

func (r *PoolReconciler) scalePool(ctx context.Context, pool *sandboxv1alpha1.Pool, args *scaleArgs) error {
	log := logf.FromContext(ctx)
	errs := make([]error, 0)
	pods := args.pods
	if satisfied, unsatisfiedDuration, dirtyPods := PoolScaleExpectations.SatisfiedExpectations(controllerutils.GetControllerKey(pool)); !satisfied {
		if unsatisfiedDuration >= expectations.ExpectationTimeout {
			log.Info("Pool scale expectations timed out, clearing stale expectations",
				"unsatisfiedDuration", unsatisfiedDuration, "dirtyPods", dirtyPods)
			PoolScaleExpectations.DeleteExpectations(controllerutils.GetControllerKey(pool))
		} else {
			log.Info("Pool scale is not ready, requeue", "unsatisfiedDuration", unsatisfiedDuration, "dirtyPods", dirtyPods)
			return fmt.Errorf("pool scale is not ready, %v", pool.Name)
		}
	}
	schedulableCnt := int32(len(args.pods))
	totalPodCnt := args.totalPodCnt
	allocatedCnt := args.allocatedCnt
	supplyCnt := args.supplyCnt
	toDeletePods := args.toDeletePods
	bufferCnt := schedulableCnt - allocatedCnt

	// Calculate desired buffer cnt.
	desiredBufferCnt := bufferCnt
	if bufferCnt < pool.Spec.CapacitySpec.BufferMin || bufferCnt > pool.Spec.CapacitySpec.BufferMax {
		desiredBufferCnt = (pool.Spec.CapacitySpec.BufferMin + pool.Spec.CapacitySpec.BufferMax) / 2
	}

	// Calculate desired schedulable cnt.
	desiredSchedulableCnt := max(allocatedCnt+supplyCnt+desiredBufferCnt, pool.Spec.CapacitySpec.PoolMin)
	// Enforce PoolMax: limit new pods based on total running pods (including evicting).
	maxNewPods := max(pool.Spec.CapacitySpec.PoolMax-totalPodCnt, 0)

	log.Info("Scale pool decision", "pool", pool.Name,
		"totalPodCnt", totalPodCnt, "schedulableCnt", schedulableCnt,
		"allocatedCnt", allocatedCnt, "bufferCnt", bufferCnt,
		"desiredBufferCnt", desiredBufferCnt, "supplyCnt", supplyCnt,
		"desiredSchedulableCnt", desiredSchedulableCnt, "maxNewPods", maxNewPods,
		"toDeletePods", len(toDeletePods), "idlePods", len(args.idlePods))

	// Scale-up: create new pods if needed and allowed by PoolMax
	if desiredSchedulableCnt > schedulableCnt && maxNewPods > 0 {
		createCnt := min(desiredSchedulableCnt-schedulableCnt, maxNewPods)
		scaleMaxUnavailable := r.getScaleMaxUnavailable(pool, desiredSchedulableCnt)
		notReadyCnt := r.countNotReadyPods(pods)
		limitedCreateCnt := scaleMaxUnavailable - notReadyCnt
		createCnt = max(0, min(createCnt, limitedCreateCnt))
		if createCnt > 0 {
			log.Info("Scaling up pool with constraint", "pool", pool.Name,
				"createCnt", createCnt, "scaleMaxUnavailable", scaleMaxUnavailable,
				"notReadyCnt", notReadyCnt, "desiredSchedulableCnt", desiredSchedulableCnt, "limitedCreateCnt", limitedCreateCnt)
			for range createCnt {
				if err := r.createPoolPod(ctx, pool, args.updateRevision); err != nil {
					log.Error(err, "Failed to create pool pod")
					errs = append(errs, err)
				}
			}
		}
	}

	// Scale-down: delete redundant or excess pods
	scaleIn := int32(0)
	if desiredSchedulableCnt < schedulableCnt {
		scaleIn = schedulableCnt - desiredSchedulableCnt
	}
	if scaleIn > 0 || len(toDeletePods) > 0 {
		podsToDelete := r.pickPodsToDelete(pods, args.idlePods, args.toDeletePods, scaleIn)
		log.Info("Scaling down pool", "pool", pool.Name, "scaleIn", scaleIn, "toDeletePods", len(toDeletePods), "podsToDelete", len(podsToDelete))
		for _, pod := range podsToDelete {
			log.Info("Deleting pool pod", "pool", pool.Name, "pod", pod.Name)
			if err := r.Delete(ctx, pod); err != nil {
				log.Error(err, "Failed to delete pool pod", "pod", pod.Name)
				errs = append(errs, err)
			}
		}
	}
	return gerrors.Join(errs...)
}

func (r *PoolReconciler) updatePoolStatus(ctx context.Context, updateRevision string, pool *sandboxv1alpha1.Pool, pods []*corev1.Pod, schedulePods []*corev1.Pod, podAllocation map[string]string) error {
	oldStatus := pool.Status.DeepCopy()
	availableCnt := int32(0)
	for _, pod := range schedulePods {
		if _, ok := podAllocation[pod.Name]; ok {
			continue
		}
		if !utils.IsPodReady(pod) {
			continue
		}
		availableCnt++
	}
	updatedCnt := int32(0)
	for _, pod := range pods {
		if pod.Labels[LabelPoolRevision] == updateRevision {
			updatedCnt++
		}
	}
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Total = int32(len(pods))
	pool.Status.Allocated = int32(len(podAllocation))
	pool.Status.Available = availableCnt
	pool.Status.Revision = updateRevision
	pool.Status.Updated = updatedCnt
	if equality.Semantic.DeepEqual(*oldStatus, pool.Status) {
		return nil
	}
	log := logf.FromContext(ctx)
	log.Info("Update pool status", "ObservedGeneration", pool.Status.ObservedGeneration, "Total", pool.Status.Total,
		"Allocated", pool.Status.Allocated, "Available", pool.Status.Available, "Revision", pool.Status.Revision, "Updated", pool.Status.Updated)
	if err := r.Status().Update(ctx, pool); err != nil {
		return err
	}
	return nil
}

func (r *PoolReconciler) pickPodsToDelete(pods []*corev1.Pod, idlePodNames []string, toDeletePodNames []string, scaleIn int32) []*corev1.Pod {
	podMap := make(map[string]*corev1.Pod)
	for _, pod := range pods {
		podMap[pod.Name] = pod
	}

	var podsToDelete []*corev1.Pod
	for _, name := range toDeletePodNames {
		pod, ok := podMap[name]
		if !ok {
			continue
		}
		podsToDelete = append(podsToDelete, pod)
	}

	var idlePods []*corev1.Pod
	for _, name := range idlePodNames {
		pod, ok := podMap[name]
		if !ok {
			continue
		}
		idlePods = append(idlePods, pod)
	}
	sort.Slice(idlePods, func(i, j int) bool {
		return idlePods[i].CreationTimestamp.Before(&idlePods[j].CreationTimestamp)
	})
	for _, pod := range idlePods {
		if scaleIn <= 0 {
			break
		}
		if pod.DeletionTimestamp == nil {
			podsToDelete = append(podsToDelete, pod)
		}
		scaleIn -= 1
	}
	return podsToDelete
}

// getScaleMaxUnavailable returns the resolved maxUnavailable value.
// If not specified, defaults to 25% of desiredTotal.
// Minimum return value is 1 to ensure scaling progress.
func (r *PoolReconciler) getScaleMaxUnavailable(pool *sandboxv1alpha1.Pool, desiredTotal int32) int32 {
	defaultPercentage := intstr.FromString("25%")

	maxUnavailable := &defaultPercentage
	if pool.Spec.ScaleStrategy != nil && pool.Spec.ScaleStrategy.MaxUnavailable != nil {
		maxUnavailable = pool.Spec.ScaleStrategy.MaxUnavailable
	}

	result, err := intstr.GetScaledValueFromIntOrPercent(maxUnavailable, int(desiredTotal), true)
	if err != nil || result < 1 {
		result = 1
	}
	return int32(result)
}

// countNotReadyPods returns the count of pods that are not ready.
// A pod is considered not ready if it doesn't have a Ready condition
// with status True.
func (r *PoolReconciler) countNotReadyPods(pods []*corev1.Pod) int32 {
	var count int32
	for _, pod := range pods {
		if !utils.IsPodReady(pod) {
			count++
		}
	}
	return count
}

func (r *PoolReconciler) createPoolPod(ctx context.Context, pool *sandboxv1alpha1.Pool, updateRevision string) error {
	log := logf.FromContext(ctx)
	pod, err := utils.GetPodFromTemplate(pool.Spec.Template, pool, metav1.NewControllerRef(pool, sandboxv1alpha1.SchemeBuilder.GroupVersion.WithKind("Pool")))
	if err != nil {
		return err
	}
	pod.Namespace = pool.Namespace
	pod.Name = ""
	pod.GenerateName = pool.Name + "-"
	pod.Labels[LabelPoolName] = pool.Name
	pod.Labels[LabelPoolRevision] = updateRevision
	if err := ctrl.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil {
		r.Recorder.Eventf(pool, corev1.EventTypeWarning, "FailedCreate", "Failed to create pool pod: %v", err)
		return err
	}
	PoolScaleExpectations.ExpectScale(controllerutils.GetControllerKey(pool), expectations.Create, pod.Name)
	log.Info("Created pool pod", "pool", pool.Name, "pod", pod.Name, "revision", updateRevision)
	r.Recorder.Eventf(pool, corev1.EventTypeNormal, "SuccessfulCreate", "Created pool pod: %v", pod.Name)
	return nil
}

// handleEviction fetches the current allocation, evicts idle pods marked for eviction,
// and returns the schedulable pods (excluding evicting idle pods) along with any eviction error.
// Eviction errors are non-fatal: they are returned to trigger a requeue but do not block the current reconcile.
func (r *PoolReconciler) handleEviction(ctx context.Context, pool *sandboxv1alpha1.Pool, pods []*corev1.Pod) ([]*corev1.Pod, error) {
	log := logf.FromContext(ctx)

	podAllocation, err := r.Allocator.GetPoolAllocation(ctx, pool)
	if err != nil {
		log.Error(err, "Failed to get pool allocation")
		return nil, err
	}

	handler := eviction.NewEvictionHandler(ctx, r.Client, pool)

	var evictionErrs []error
	filtered := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if !handler.NeedsEviction(pod) {
			filtered = append(filtered, pod)
			continue
		}

		if sandboxName, allocated := podAllocation[pod.Name]; allocated {
			log.V(1).Info("Skipping eviction for allocated pod", "pod", pod.Name, "sandbox", sandboxName)
			filtered = append(filtered, pod)
			continue
		}

		// Idle pod marked for eviction: evict and exclude from scheduling
		log.Info("Evicting idle pool pod", "pool", pool.Name, "pod", pod.Name)
		if err := handler.Evict(ctx, pod); err != nil {
			log.Error(err, "Failed to evict pod", "pod", pod.Name)
			evictionErrs = append(evictionErrs, fmt.Errorf("failed to evict pod %s: %w", pod.Name, err))
		} else {
			r.Recorder.Eventf(pool, corev1.EventTypeNormal, "PodEvicted", "Evicted idle pod: %s", pod.Name)
		}
	}

	return filtered, gerrors.Join(evictionErrs...)
}
