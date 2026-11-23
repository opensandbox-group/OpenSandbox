// Copyright 2025 The OpenSandbox Authors
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
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	algorithm "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	taskapi "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type allocationStore interface {
	GetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (*poolAllocation, error)
	SetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool, allocation *poolAllocation) error
	ClearAllocation(ctx context.Context, ns string, poolName string) error
	ReleaseAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string)
	UpdateAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string)
	// ReleaseSandboxAllocation releases all pods allocated to the given sandbox name.
	// This is used when a BatchSandbox is deleted and its allocation needs to be cleaned up.
	ReleaseSandboxAllocation(ctx context.Context, ns string, poolName string, sandboxName string)
	Recover(ctx context.Context, c client.Client) error
	OwnsPods(ns, poolName, sandboxName string, pods []string) bool
}

// poolEntry represents a single pool's allocation data with its own lock for fine-grained concurrency control
type poolEntry struct {
	mu   sync.RWMutex
	data map[string]string // podName -> sandboxName
}

// inMemoryAllocationStore depends on annoAllocationSyncer to get allocation info from BatchSandbox.
type inMemoryAllocationStore struct {
	poolsMu sync.RWMutex
	pools   map[string]*poolEntry
	syncer  *annoAllocationSyncer
}

func newInMemoryAllocationStore() allocationStore {
	return &inMemoryAllocationStore{
		pools:  make(map[string]*poolEntry),
		syncer: &annoAllocationSyncer{},
	}
}

// Recover builds the allocation map from all BatchSandboxes
// This should be called once during controller initialization before reconcile starts
func (store *inMemoryAllocationStore) Recover(ctx context.Context, c client.Client) error {
	log := logf.FromContext(ctx)
	log.Info("Starting allocation recovery from BatchSandboxes")

	batchSandboxList := &sandboxv1alpha1.BatchSandboxList{}
	if err := c.List(ctx, batchSandboxList); err != nil {
		return fmt.Errorf("failed to list batch sandboxes for recovery: %w", err)
	}

	// Build new pools map first without holding the lock
	newPools := make(map[string]*poolEntry)
	// Conflicting annotations must recover the same owner regardless of list order.
	sort.Slice(batchSandboxList.Items, func(i, j int) bool {
		a, b := batchSandboxList.Items[i], batchSandboxList.Items[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})

	for _, sbx := range batchSandboxList.Items {
		poolRef := sbx.Spec.PoolRef
		if poolRef == "" {
			continue
		}
		allocation, err := store.syncer.GetAllocation(ctx, &sbx)
		if err != nil {
			log.Error(err, "Failed to unmarshal sandbox allocation during recovery", "sandbox", sbx.Name)
			return err
		}
		key := store.poolKey(sbx.Namespace, poolRef)
		entry, exists := newPools[key]
		if !exists {
			entry = &poolEntry{
				data: make(map[string]string),
			}
			newPools[key] = entry
		}

		// Filter pods that have already been released (alloc-released records completed recycle).
		// alloc-release (in-progress) pods must NOT be filtered: the recycle handler is still
		// processing them and they are still "in use" from the pool's perspective.
		allocReleased, err := store.syncer.GetReleased(ctx, &sbx)
		if err != nil {
			log.Error(err, "Failed to unmarshal sandbox released during recovery", "sandbox", sbx.Name)
			return err
		}
		releasedSet := sets.New(allocReleased.Pods...)
		for _, podName := range allocation.Pods {
			if !releasedSet.Has(podName) {
				entry.data[podName] = sbx.Name
			}
		}

		log.Info("Recovered sandbox allocation", "pool", poolRef, "sandbox", sbx.Name, "pods", len(allocation.Pods))
	}

	store.poolsMu.Lock()
	store.pools = newPools
	store.poolsMu.Unlock()

	log.Info("Allocation recovery completed", "totalPools", len(store.pools))
	return nil
}

func (store *inMemoryAllocationStore) ClearAllocation(ctx context.Context, ns string, poolName string) error {
	log := logf.FromContext(ctx)
	store.poolsMu.Lock()
	log.Info("Clearing pool allocation", "namespace", ns, "pool", poolName)
	delete(store.pools, store.poolKey(ns, poolName))
	store.poolsMu.Unlock()
	return nil
}

func (store *inMemoryAllocationStore) GetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (*poolAllocation, error) {
	store.poolsMu.RLock()
	entry, exists := store.pools[store.poolKey(pool.Namespace, pool.Name)]
	store.poolsMu.RUnlock()

	alloc := &poolAllocation{
		PodAllocation: make(map[string]string),
	}

	if !exists {
		return alloc, nil
	}

	entry.mu.RLock()
	defer entry.mu.RUnlock()

	for podName, sandboxName := range entry.data {
		alloc.PodAllocation[podName] = sandboxName
	}

	return alloc, nil
}

func (store *inMemoryAllocationStore) OwnsPods(ns, poolName, sandboxName string, pods []string) bool {
	store.poolsMu.RLock()
	entry := store.pools[store.poolKey(ns, poolName)]
	store.poolsMu.RUnlock()
	if entry == nil {
		return false
	}
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	for _, pod := range pods {
		if owner, found := entry.data[pod]; !found || owner != sandboxName {
			return false
		}
	}
	return true
}

func (store *inMemoryAllocationStore) SetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool, alloc *poolAllocation) error {
	entry := store.getOrCreatePool(pool.Namespace, pool.Name)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.data = make(map[string]string)
	for podName, sandboxName := range alloc.PodAllocation {
		entry.data[podName] = sandboxName
	}

	return nil
}

func (store *inMemoryAllocationStore) ReleaseAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string) {
	entry := store.getOrCreatePool(ns, poolName)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	for _, podName := range pods {
		if sandboxName == "" || entry.data[podName] == sandboxName {
			delete(entry.data, podName)
		}
	}
}

func (store *inMemoryAllocationStore) UpdateAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string) {
	entry := store.getOrCreatePool(ns, poolName)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	for podName, sbxName := range entry.data {
		if sbxName == sandboxName {
			delete(entry.data, podName)
		}
	}

	for _, podName := range pods {
		// Stale annotations and rollback must not take a Pod from another sandbox.
		if _, reserved := entry.data[podName]; !reserved {
			entry.data[podName] = sandboxName
		}
	}
}

// ReleaseSandboxAllocation releases all pods allocated to the given sandbox from the in-memory store.
// This should be called when a BatchSandbox is deleted to ensure the allocation state is cleaned up.
func (store *inMemoryAllocationStore) ReleaseSandboxAllocation(ctx context.Context, ns string, poolName string, sandboxName string) {
	store.poolsMu.RLock()
	entry, exists := store.pools[store.poolKey(ns, poolName)]
	store.poolsMu.RUnlock()

	if !exists {
		return
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	for podName, sbxName := range entry.data {
		if sbxName == sandboxName {
			delete(entry.data, podName)
		}
	}
}

// getOrCreatePool returns the pool entry for the given pool name, creating it if necessary.
// This method uses a double-checked locking pattern to ensure thread-safe creation.
func (store *inMemoryAllocationStore) getOrCreatePool(ns string, poolName string) *poolEntry {
	store.poolsMu.RLock()
	entry, exists := store.pools[store.poolKey(ns, poolName)]
	store.poolsMu.RUnlock()

	if exists {
		return entry
	}

	store.poolsMu.Lock()
	defer store.poolsMu.Unlock()

	// Double-check after acquiring write lock
	if entry, exists := store.pools[store.poolKey(ns, poolName)]; exists {
		return entry
	}

	entry = &poolEntry{
		data: make(map[string]string),
	}
	store.pools[store.poolKey(ns, poolName)] = entry
	return entry
}

func (store *inMemoryAllocationStore) poolKey(ns, name string) string {
	return ns + "/" + name
}

type allocationSyncer interface {
	SetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, allocation *sandboxAllocation) error
	GetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*sandboxAllocation, error)
	GetRelease(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*allocationRelease, error)
	SetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, released *allocationReleased) (bool, error)
	GetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*allocationReleased, error)
}

type annoAllocationSyncer struct {
	client    client.Client
	apiReader client.Reader
}

func newAnnoAllocationSyncer(c client.Client, apiReader client.Reader) allocationSyncer {
	return &annoAllocationSyncer{client: c, apiReader: apiReader}
}

func (syncer *annoAllocationSyncer) SetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, allocation *sandboxAllocation) error {
	allocation.PoolRef = sandbox.Spec.PoolRef
	allocation.Generation = sandbox.Generation
	js, err := json.Marshal(allocation)
	if err != nil {
		return err
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		anno = make(map[string]string)
	}
	anno[annoAllocStatusKey] = string(js)
	sandbox.SetAnnotations(anno)

	needAddFinalizer := !controllerutil.ContainsFinalizer(sandbox, finalizerPoolAllocation)
	if needAddFinalizer {
		sandbox.SetFinalizers(append(sandbox.GetFinalizers(), finalizerPoolAllocation))
	}

	meta := map[string]any{
		"annotations": map[string]string{
			annoAllocStatusKey: string(js),
		},
	}
	if needAddFinalizer {
		meta["finalizers"] = sandbox.GetFinalizers()
	}
	patchData, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return err
	}
	obj := &sandboxv1alpha1.BatchSandbox{}
	obj.Name = sandbox.Name
	obj.Namespace = sandbox.Namespace
	persistStart := time.Now()
	patchErr := syncer.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData))
	recordAllocatorPersistAllocStateDuration(ctx, sandbox, time.Since(persistStart), patchErr)
	return patchErr
}

func (syncer *annoAllocationSyncer) GetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*sandboxAllocation, error) {
	allocation := &sandboxAllocation{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return allocation, nil
	}
	if raw := anno[annoAllocStatusKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), allocation)
		if err != nil {
			return nil, err
		}
	}
	return allocation, nil
}

func (syncer *annoAllocationSyncer) GetRelease(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*allocationRelease, error) {
	release := &allocationRelease{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return release, nil
	}
	if raw := anno[annoAllocReleaseKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), release)
		if err != nil {
			return nil, err
		}
	}
	return release, nil
}

func (syncer *annoAllocationSyncer) GetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*allocationReleased, error) {
	released := &allocationReleased{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return released, nil
	}
	if raw := anno[annoAllocReleasedKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), released)
		if err != nil {
			return nil, err
		}
	}
	return released, nil
}

func (syncer *annoAllocationSyncer) SetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, released *allocationReleased) (bool, error) {
	if syncer.apiReader == nil {
		return false, fmt.Errorf("uncached API reader is not configured")
	}
	js, err := json.Marshal(released)
	if err != nil {
		return false, err
	}
	// Finalizers are shared with other cleanup controllers. Recompute from the
	// latest object on every optimistic-lock retry so a stale array cannot
	// restore a finalizer another controller has already removed. Bypass the
	// informer cache so every retry can observe the conflicting API write.
	applied := false
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := syncer.apiReader.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if sandbox.UID != "" && sandbox.UID != latest.UID {
			return nil // The original sandbox has already been deleted.
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations[annoAllocReleasedKey] = string(js)
		if !latest.DeletionTimestamp.IsZero() {
			allocation, err := syncer.GetAllocation(ctx, latest)
			if err != nil {
				return err
			}
			releasedSet := sets.New(released.Pods...)
			if releasedSet.HasAll(allocation.Pods...) {
				controllerutil.RemoveFinalizer(latest, finalizerPoolAllocation)
			}
		}
		if err := syncer.client.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.DeepCopyInto(sandbox)
		applied = true
		return nil
	})
	return applied, err
}

type allocSpec struct {
	// Sandboxes contains all BatchSandboxes to be scheduled in this round.
	Sandboxes []*sandboxv1alpha1.BatchSandbox
	Pool      *sandboxv1alpha1.Pool
	// Pods contains all candidate pods owned by the pool.
	Pods []*corev1.Pod
}

type taskAllocationState struct {
	mu        sync.RWMutex
	ready     bool
	sandboxes map[string]types.UID
}

type Allocator interface {
	Schedule(ctx context.Context, spec *allocSpec) (*algorithm.AllocAction, error)
	GetPoolAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (map[string]string, error)
	ClearPoolAllocation(ctx context.Context, ns string, poolName string) error
	// ReleasePodsAllocation releases the in-memory allocation for the given pods directly,
	// without persisting to an annotation. Used for orphan pods whose sandbox no longer exists.
	ReleasePodsAllocation(ctx context.Context, ns string, poolName string, pods []string)
	SyncSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error
	SyncSandboxReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error
	GetSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) ([]string, error)
	GetSandboxReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) ([]string, error)
}

type defaultAllocator struct {
	store           allocationStore
	syncer          allocationSyncer
	client          client.Client
	apiReader       client.Reader
	algorithm       algorithm.Algorithm
	recoverOnce     sync.Once
	taskClient      func(*corev1.Pod) *taskapi.Client
	taskAllocations sync.Map
}

// NewDefaultAllocator requires an uncached reader for release/finalizer retries.
func NewDefaultAllocator(c client.Client, apiReader client.Reader) *defaultAllocator {
	return &defaultAllocator{
		store:     newInMemoryAllocationStore(),
		syncer:    newAnnoAllocationSyncer(c, apiReader),
		client:    c,
		apiReader: apiReader,
		algorithm: &algorithm.PackedSchedule{},
		taskClient: func(pod *corev1.Pod) *taskapi.Client {
			return taskapi.NewClient("http://" + net.JoinHostPort(pod.Status.PodIP, "5758"))
		},
	}
}

func (allocator *defaultAllocator) taskAllocationState(namespace, poolName string) *taskAllocationState {
	key := namespace + "/" + poolName
	if state, found := allocator.taskAllocations.Load(key); found {
		return state.(*taskAllocationState)
	}
	state, _ := allocator.taskAllocations.LoadOrStore(key, &taskAllocationState{})
	return state.(*taskAllocationState)
}

func (allocator *defaultAllocator) CheckSandboxAllocation(_ context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error {
	state := allocator.taskAllocationState(sandbox.Namespace, sandbox.Spec.PoolRef)
	state.mu.RLock()
	defer state.mu.RUnlock()
	if !state.ready {
		return fmt.Errorf("pool %s allocation is not confirmed yet", sandbox.Spec.PoolRef)
	}
	if uid, found := state.sandboxes[sandbox.Name]; !found || uid != sandbox.UID {
		return fmt.Errorf("sandbox %s allocation identity is not confirmed", sandbox.Name)
	}
	if !allocator.store.OwnsPods(sandbox.Namespace, sandbox.Spec.PoolRef, sandbox.Name, pods) {
		return fmt.Errorf("sandbox %s no longer owns its task pods", sandbox.Name)
	}
	return nil
}

// Confirm the cached recovery input once before authorizing task access.
// A partially observed set of claims must not select an owner during startup.
func (allocator *defaultAllocator) confirmRecoveredSandboxes(ctx context.Context, spec *allocSpec) error {
	sandboxes := &sandboxv1alpha1.BatchSandboxList{}
	if err := allocator.apiReader.List(ctx, sandboxes, client.InNamespace(spec.Pool.Namespace)); err != nil {
		return err
	}
	cached := make(map[string]*sandboxv1alpha1.BatchSandbox, len(spec.Sandboxes))
	for _, sandbox := range spec.Sandboxes {
		cached[sandbox.Name] = sandbox
	}
	seen := 0
	for i := range sandboxes.Items {
		latest := &sandboxes.Items[i]
		if latest.Spec.PoolRef != spec.Pool.Name || latest.Spec.Template != nil {
			continue
		}
		observed := cached[latest.Name]
		if observed == nil || observed.UID != latest.UID || observed.Generation != latest.Generation {
			return fmt.Errorf("pool %s allocation recovery is waiting for the sandbox cache", spec.Pool.Name)
		}
		for _, key := range []string{annoAllocStatusKey, annoAllocReleaseKey, annoAllocReleasedKey} {
			if observed.Annotations[key] != latest.Annotations[key] {
				return fmt.Errorf("pool %s allocation recovery is waiting for the sandbox cache", spec.Pool.Name)
			}
		}
		seen++
	}
	if seen != len(cached) {
		return fmt.Errorf("pool %s allocation recovery is waiting for the sandbox cache", spec.Pool.Name)
	}
	return nil
}

func (allocator *defaultAllocator) Schedule(ctx context.Context, spec *allocSpec) (*algorithm.AllocAction, error) {
	state := allocator.taskAllocationState(spec.Pool.Namespace, spec.Pool.Name)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sandboxes == nil {
		if err := allocator.confirmRecoveredSandboxes(ctx, spec); err != nil {
			return nil, err
		}
	}

	log := logf.FromContext(ctx)
	log.Info("Schedule started", "pool", spec.Pool.Name, "totalPods", len(spec.Pods), "sandboxes", len(spec.Sandboxes))
	if err := allocator.checkRecovery(ctx); err != nil {
		return nil, err
	}

	// Fetch pool allocation once and reuse it for both stale-sandbox cleanup and available-pod filtering.
	// This avoids a double store read on every reconcile.
	podAllocation, err := allocator.GetPoolAllocation(ctx, spec.Pool)
	if err != nil {
		return nil, err
	}

	claims, err := allocationClaims(ctx, spec.Sandboxes)
	if err != nil {
		return nil, err
	}
	if state.sandboxes == nil {
		existing := sets.New[string]()
		for _, sandbox := range spec.Sandboxes {
			existing.Insert(sandbox.Name)
		}
		recovered := make(map[string]string)
		for pod, owner := range podAllocation {
			if !existing.Has(owner) && len(claims[pod]) == 0 {
				recovered[pod] = owner
			} // retain orphan cleanup
		}
		for pod, claimants := range claims {
			recovered[pod] = preferredAllocationOwner(claimants, podAllocation[pod])
		}
		podAllocation = recovered
	}
	// A failed repair must suspend the conflicting claimants without preventing
	// unrelated, confirmed sandboxes from running or cleaning up their tasks.
	for _, claimants := range claims {
		if len(claimants) > 1 {
			for _, sandbox := range claimants {
				delete(state.sandboxes, sandbox.Name)
			}
		}
	}
	if err := allocator.resolveTaskAllocationConflicts(ctx, spec.Pool, claims, podAllocation); err != nil {
		return nil, err
	}
	if err := allocator.store.SetAllocation(ctx, spec.Pool, &poolAllocation{PodAllocation: podAllocation}); err != nil {
		return nil, err
	}

	for _, sandbox := range spec.Sandboxes {
		if err := allocator.repairSandboxAllocation(ctx, sandbox, podAllocation); err != nil {
			return nil, err
		}
	}

	// GC + per-sandbox allocation requests: build SandboxRequests for all existing sandboxes and
	// append orphan entries for pods whose sandbox no longer exists (e.g. force-deleted).
	// Orphan entries carry PodSupplement=0 and ToRelease=orphan pods so the normal recycle path
	// handles them without any special-casing outside this function.
	// Terminating sandboxes are handled inside getSandboxRequest: they receive no new supplement and
	// task cleanup hands back pods before they can be recycled.
	allRequest, err := allocator.getAllRequest(ctx, spec.Sandboxes, podAllocation)
	if err != nil {
		return nil, err
	}

	// Build available pod list using the already-fetched allocation to avoid an extra store read.
	availablePods, err := allocator.getAvailablePodsFromAlloc(ctx, podAllocation, spec.Pods)
	if err != nil {
		return nil, err
	}

	// Run the allocation algorithm.
	action := allocator.algorithm.Schedule(availablePods, allRequest)
	state.sandboxes = make(map[string]types.UID, len(spec.Sandboxes))
	for _, sandbox := range spec.Sandboxes {
		state.sandboxes[sandbox.Name] = sandbox.UID
	}
	state.ready = true
	return action, nil
}

// getAllRequest builds per-sandbox allocation requests for all existing sandboxes and appends
// orphan entries for pods in podAllocation whose sandbox is no longer in the sandboxes list
// (e.g. force-deleted). Orphan entries carry PodSupplement=0 and ToRelease set to the orphan
// pods so the normal recycle path handles them without special-casing in the caller.
func (allocator *defaultAllocator) getAllRequest(ctx context.Context, sandboxes []*sandboxv1alpha1.BatchSandbox, podAllocation map[string]string) ([]*algorithm.SandboxRequest, error) {
	log := logf.FromContext(ctx)
	existingSandboxes := make(map[string]struct{}, len(sandboxes))
	allRequest := make([]*algorithm.SandboxRequest, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		existingSandboxes[sandbox.Name] = struct{}{}
		request, err := allocator.getSandboxRequest(ctx, sandbox, podAllocation)
		if err != nil {
			return nil, err
		}
		allRequest = append(allRequest, request)
	}
	orphanToRelease := make(map[string][]string)
	for podName, sandboxName := range podAllocation {
		if _, exists := existingSandboxes[sandboxName]; !exists {
			orphanToRelease[sandboxName] = append(orphanToRelease[sandboxName], podName)
		}
	}
	for sandboxName, pods := range orphanToRelease {
		log.Info("GC: queuing orphan pods for release", "sandbox", sandboxName, "pods", pods)
		allRequest = append(allRequest, &algorithm.SandboxRequest{
			SandboxName:   sandboxName,
			PodSupplement: 0,
			ToRelease:     pods,
		})
	}
	return allRequest, nil
}

// allocationClaims excludes completed handbacks but retains pods still being cleaned up.
func allocationClaims(ctx context.Context, sandboxes []*sandboxv1alpha1.BatchSandbox) (map[string][]*sandboxv1alpha1.BatchSandbox, error) {
	claims := make(map[string][]*sandboxv1alpha1.BatchSandbox)
	syncer := &annoAllocationSyncer{}
	for _, sandbox := range sandboxes {
		allocated, err := syncer.GetAllocation(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		released, err := syncer.GetReleased(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		for pod := range sets.New(allocated.Pods...).Difference(sets.New(released.Pods...)) {
			claims[pod] = append(claims[pod], sandbox)
		}
	}
	return claims, nil
}

func preferredAllocationOwner(claimants []*sandboxv1alpha1.BatchSandbox, current string) string {
	owner := claimants[0].Name
	for _, sandbox := range claimants {
		if sandbox.Name == current {
			return current
		}
		if sandbox.Name > owner {
			owner = sandbox.Name
		}
	}
	return owner
}

func (allocator *defaultAllocator) resolveTaskAllocationConflicts(ctx context.Context, pool *sandboxv1alpha1.Pool, claims map[string][]*sandboxv1alpha1.BatchSandbox, owners map[string]string) error {
	resolved := make(map[string]string)
	for podName, claimants := range claims {
		if _, reserved := owners[podName]; !reserved {
			// Persisted claims can arrive after the one-time store recovery.
			resolved[podName] = preferredAllocationOwner(claimants, "")
		}
		if len(claimants) < 2 {
			continue
		}
		hasTasks := false
		for _, sandbox := range claimants {
			hasTasks = hasTasks || sandbox.Spec.TaskTemplate != nil || controllerutil.ContainsFinalizer(sandbox, finalizerTaskCleanup)
		}
		if !hasTasks {
			continue
		}
		pod := &corev1.Pod{}
		if err := allocator.apiReader.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: podName}, pod); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return err
		}
		if pod.Status.PodIP == "" {
			return fmt.Errorf("cannot determine task owner for conflicting pod %s: no IP", podName)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		tasks, err := allocator.taskClient(pod).List(probeCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read tasks for conflicting pod %s: %w", podName, err)
		}
		owner := ""
		for _, task := range tasks {
			taskOwner := ""
			for _, sandbox := range claimants {
				suffix, ok := strings.CutPrefix(task.Name, sandbox.Name+"-")
				index, err := strconv.Atoi(suffix)
				if ok && err == nil && index >= 0 && strconv.Itoa(index) == suffix {
					taskOwner = sandbox.Name
					break
				}
			}
			if taskOwner == "" || (owner != "" && owner != taskOwner) {
				return fmt.Errorf("cannot determine task owner for conflicting pod %s", podName)
			}
			owner = taskOwner
		}
		if owner != "" {
			resolved[podName] = owner
		}
	}
	for pod, owner := range resolved {
		owners[pod] = owner
	}
	return nil
}

func (allocator *defaultAllocator) repairSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, podAllocation map[string]string) error {
	allocated, err := allocator.GetSandboxAllocation(ctx, sandbox)
	if err != nil {
		return err
	}
	conflicting := sets.New[string]()
	for _, pod := range allocated {
		if owner, reserved := podAllocation[pod]; reserved && owner != sandbox.Name {
			conflicting.Insert(pod)
		}
	}
	if conflicting.Len() == 0 {
		return nil
	}
	released, err := allocator.GetSandboxReleased(ctx, sandbox)
	if err != nil {
		return err
	}
	conflicting = conflicting.Difference(sets.New(released...))
	if conflicting.Len() == 0 {
		return nil
	}
	retained := make([]string, 0, len(allocated))
	for _, pod := range allocated {
		if !conflicting.Has(pod) {
			retained = append(retained, pod)
		}
	}
	// Repair the reference without recycling the other sandbox's Pod.
	updated := sandbox.DeepCopy()
	raw, err := json.Marshal(sandboxAllocation{Pods: retained, PoolRef: sandbox.Spec.PoolRef, Generation: sandbox.Generation})
	if err != nil {
		return err
	}
	updated.Annotations[annoAllocStatusKey] = string(raw)
	if err := allocator.client.Patch(ctx, updated, client.MergeFromWithOptions(sandbox, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	updated.DeepCopyInto(sandbox)
	return nil
}

func (allocator *defaultAllocator) getSandboxRequest(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, podAllocation map[string]string) (*algorithm.SandboxRequest, error) {
	allocated, err := allocator.GetSandboxAllocation(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	released, err := allocator.GetSandboxReleased(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	deleting := !sandbox.DeletionTimestamp.IsZero()
	var release []string
	if deleting && !controllerutil.ContainsFinalizer(sandbox, finalizerTaskCleanup) {
		// No tasks remain: hand back all outstanding allocations.
		release = allocated
	} else {
		// While task cleanup owns the pods, only recycle explicit handoffs.
		// The same annotation carries normal task completion and pause releases.
		release, err = allocator.getSandboxRelease(ctx, sandbox)
		if err != nil {
			return nil, err
		}
	}
	releasedSet := sets.New(released...)
	toRelease := make([]string, 0, len(release))
	allocatedSet := sets.New(allocated...)
	for _, pod := range release {
		if podAllocation[pod] != sandbox.Name || !allocatedSet.Has(pod) {
			continue
		}
		if !releasedSet.Has(pod) {
			toRelease = append(toRelease, pod)
		}
	}
	var supplement int32
	if !deleting && sandbox.Spec.Replicas != nil {
		supplement = max(*sandbox.Spec.Replicas-int32(len(allocated)), 0)
	}
	return &algorithm.SandboxRequest{
		SandboxName:   sandbox.Name,
		CurAllocation: allocated,
		CurReleased:   released,
		PodSupplement: supplement,
		ToRelease:     toRelease,
	}, nil
}

func (allocator *defaultAllocator) GetSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	allocation, err := allocator.syncer.GetAllocation(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return allocation.Pods, nil
}

func (allocator *defaultAllocator) getSandboxRelease(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	release, err := allocator.syncer.GetRelease(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return release.Pods, nil
}

func (allocator *defaultAllocator) GetSandboxReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	released, err := allocator.syncer.GetReleased(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return released.Pods, nil
}

func (allocator *defaultAllocator) GetPoolAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (map[string]string, error) {
	// Eviction and the capacity metrics read the store before the first Schedule.
	if err := allocator.checkRecovery(ctx); err != nil {
		return nil, err
	}
	alloc, err := allocator.store.GetAllocation(ctx, pool)
	if err != nil {
		return nil, err
	}
	if alloc == nil {
		return map[string]string{}, nil
	}
	return alloc.PodAllocation, nil
}

func (allocator *defaultAllocator) ClearPoolAllocation(ctx context.Context, ns string, poolName string) error {
	state := allocator.taskAllocationState(ns, poolName)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.ready = false
	state.sandboxes = nil

	return allocator.store.ClearAllocation(ctx, ns, poolName)
}

func (allocator *defaultAllocator) ReleasePodsAllocation(ctx context.Context, ns string, poolName string, pods []string) {
	state := allocator.taskAllocationState(ns, poolName)
	state.mu.Lock()
	defer state.mu.Unlock()

	allocator.store.ReleaseAllocation(ctx, ns, poolName, "", pods)
}

// SyncSandboxAllocation updates the in-memory allocation store and then persists to the sandbox annotation.
// If annotation sync fails, the in-memory store is rolled back to the previous state to maintain consistency.
func (allocator *defaultAllocator) SyncSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error {
	state := allocator.taskAllocationState(sandbox.Namespace, sandbox.Spec.PoolRef)
	state.mu.Lock()
	defer state.mu.Unlock()

	log := logf.FromContext(ctx)
	log.Info("Syncing sandbox allocation", "sandbox", sandbox.Name, "pods", pods)
	poolRef := sandbox.Spec.PoolRef

	// Snapshot the current in-memory state for rollback on failure.
	oldState, err := allocator.syncer.GetAllocation(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("failed to get current sandbox allocation: %w", err)
	}
	released, err := allocator.syncer.GetReleased(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("failed to get sandbox released pods: %w", err)
	}
	releasedSet := sets.New(released.Pods...)

	// Phase 1: update in-memory store optimistically.
	allocator.store.UpdateAllocation(ctx, sandbox.Namespace, poolRef, sandbox.Name, sets.List(sets.New(pods...).Difference(releasedSet)))

	// Phase 2: persist to sandbox annotation.
	allocation := &sandboxAllocation{Pods: pods}
	if err := allocator.syncer.SetAllocation(ctx, sandbox, allocation); err != nil {
		// Rollback in-memory store to the previous state.
		log.Error(err, "Rollback sandbox allocation", "sandbox", sandbox.Name, "pods", oldState.Pods)
		allocator.store.UpdateAllocation(ctx, sandbox.Namespace, poolRef, sandbox.Name, sets.List(sets.New(oldState.Pods...).Difference(releasedSet)))
		return err
	}
	if state.sandboxes != nil {
		state.sandboxes[sandbox.Name] = sandbox.UID
	}
	return nil
}

// SyncSandboxReleased persists the released state to the sandbox annotation and then releases from the in-memory store.
// Annotation must succeed before the pods are removed from the in-memory store to prevent pods from being
// re-allocated before the release is durably committed.
func (allocator *defaultAllocator) SyncSandboxReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error {
	state := allocator.taskAllocationState(sandbox.Namespace, sandbox.Spec.PoolRef)
	state.mu.Lock()
	defer state.mu.Unlock()

	log := logf.FromContext(ctx)
	log.Info("Syncing sandbox released", "sandbox", sandbox.Name, "pods", pods)
	poolRef := sandbox.Spec.PoolRef

	// Phase 1: persist to sandbox annotation.
	released := &allocationReleased{Pods: pods}
	applied, err := allocator.syncer.SetReleased(ctx, sandbox, released)
	if err != nil {
		log.Error(err, "Failed to sync sandbox released", "sandbox", sandbox.Name, "pods", pods)
		return err
	}
	if !applied {
		// A replacement may already own some of these Pods under the same name.
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := allocator.apiReader.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		} else if latest.Spec.PoolRef == poolRef {
			allocation, err := allocator.syncer.GetAllocation(ctx, latest)
			if err != nil {
				return err
			}
			released, err := allocator.syncer.GetReleased(ctx, latest)
			if err != nil {
				return err
			}
			active := sets.New(allocation.Pods...).Difference(sets.New(released.Pods...))
			pods = sets.List(sets.New(pods...).Difference(active))
		}
		if state.sandboxes[sandbox.Name] == sandbox.UID {
			delete(state.sandboxes, sandbox.Name)
		}
	}

	// Phase 2: release remaining reservations from the in-memory store.
	allocator.store.ReleaseAllocation(ctx, sandbox.Namespace, poolRef, sandbox.Name, pods)
	return nil
}

// checkRecovery runs the one-time state recovery. If recovery fails the process
// is terminated via os.Exit(1) because the allocator cannot operate with an
// inconsistent in-memory state. The error return is kept for interface compatibility
// but will never actually be returned.
func (allocator *defaultAllocator) checkRecovery(ctx context.Context) error {
	allocator.recoverOnce.Do(func() {
		log := logf.FromContext(ctx)
		if err := allocator.store.Recover(ctx, allocator.client); err != nil {
			log.Error(err, "Fatal: allocator state recovery failed, exiting")
			os.Exit(1)
		}
	})
	return nil
}

func (allocator *defaultAllocator) getAvailablePodsFromAlloc(_ context.Context, podAllocation map[string]string, pods []*corev1.Pod) ([]string, error) {
	availablePods := make([]string, 0)
	for _, pod := range pods {
		if _, ok := podAllocation[pod.Name]; ok {
			continue
		}
		if !utils.IsPodReady(pod) {
			continue
		}
		availablePods = append(availablePods, pod.Name)
	}
	return availablePods, nil
}
