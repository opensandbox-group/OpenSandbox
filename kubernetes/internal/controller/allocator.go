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
	"os"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	algorithm "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

type AllocationStore interface {
	GetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (*PoolAllocation, error)
	SetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool, allocation *PoolAllocation) error
	ClearAllocation(ctx context.Context, ns string, poolName string) error
	ReleaseAllocation(ctx context.Context, ns string, poolName string, pods []string)
	UpdateAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string)
	// ReleaseSandboxAllocation releases all pods allocated to the given sandbox name.
	// This is used when a BatchSandbox is deleted and its allocation needs to be cleaned up.
	ReleaseSandboxAllocation(ctx context.Context, ns string, poolName string, sandboxName string)
	Recover(ctx context.Context, c client.Client) error
}

// poolEntry represents a single pool's allocation data with its own lock for fine-grained concurrency control
type poolEntry struct {
	mu   sync.RWMutex
	data map[string]string // podName -> sandboxName
}

// InMemoryAllocationStore depends on annoAllocationSyncer to get allocation info from BatchSandbox.
type InMemoryAllocationStore struct {
	poolsMu sync.RWMutex
	pools   map[string]*poolEntry
	syncer  *annoAllocationSyncer
}

func NewInMemoryAllocationStore() AllocationStore {
	return &InMemoryAllocationStore{
		pools:  make(map[string]*poolEntry),
		syncer: &annoAllocationSyncer{},
	}
}

// Recover builds the allocation map from all BatchSandboxes
// This should be called once during controller initialization before reconcile starts
func (store *InMemoryAllocationStore) Recover(ctx context.Context, c client.Client) error {
	log := logf.FromContext(ctx)
	log.Info("Starting allocation recovery from BatchSandboxes")

	batchSandboxList := &sandboxv1alpha1.BatchSandboxList{}
	if err := c.List(ctx, batchSandboxList); err != nil {
		return fmt.Errorf("failed to list batch sandboxes for recovery: %w", err)
	}

	// Build new pools map first without holding the lock
	newPools := make(map[string]*poolEntry)

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

		for _, podName := range allocation.Pods {
			entry.data[podName] = sbx.Name
		}
		// Filter pods that have already been released (alloc-released records completed recycle).
		// alloc-release (in-progress) pods must NOT be filtered: the recycle handler is still
		// processing them and they are still "in use" from the pool's perspective.
		allocReleased, err := store.syncer.GetReleased(ctx, &sbx)
		if err != nil {
			log.Error(err, "Failed to unmarshal sandbox released during recovery", "sandbox", sbx.Name)
			return err
		}
		for _, podName := range allocReleased.Pods {
			if entry.data[podName] == sbx.Name {
				delete(entry.data, podName)
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

func (store *InMemoryAllocationStore) ClearAllocation(ctx context.Context, ns string, poolName string) error {
	log := logf.FromContext(ctx)
	store.poolsMu.Lock()
	log.Info("Clearing pool allocation", "namespace", ns, "pool", poolName)
	delete(store.pools, store.poolKey(ns, poolName))
	store.poolsMu.Unlock()
	return nil
}

func (store *InMemoryAllocationStore) GetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool) (*PoolAllocation, error) {
	store.poolsMu.RLock()
	entry, exists := store.pools[store.poolKey(pool.Namespace, pool.Name)]
	store.poolsMu.RUnlock()

	alloc := &PoolAllocation{
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

func (store *InMemoryAllocationStore) SetAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool, alloc *PoolAllocation) error {
	entry := store.getOrCreatePool(pool.Namespace, pool.Name)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.data = make(map[string]string)
	for podName, sandboxName := range alloc.PodAllocation {
		entry.data[podName] = sandboxName
	}

	return nil
}

func (store *InMemoryAllocationStore) ReleaseAllocation(ctx context.Context, ns string, poolName string, pods []string) {
	entry := store.getOrCreatePool(ns, poolName)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	for _, podName := range pods {
		delete(entry.data, podName)
	}
}

func (store *InMemoryAllocationStore) UpdateAllocation(ctx context.Context, ns string, poolName string, sandboxName string, pods []string) {
	entry := store.getOrCreatePool(ns, poolName)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	for podName, sbxName := range entry.data {
		if sbxName == sandboxName {
			delete(entry.data, podName)
		}
	}

	for _, podName := range pods {
		entry.data[podName] = sandboxName
	}
}

// ReleaseSandboxAllocation releases all pods allocated to the given sandbox from the in-memory store.
// This should be called when a BatchSandbox is deleted to ensure the allocation state is cleaned up.
func (store *InMemoryAllocationStore) ReleaseSandboxAllocation(ctx context.Context, ns string, poolName string, sandboxName string) {
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
func (store *InMemoryAllocationStore) getOrCreatePool(ns string, poolName string) *poolEntry {
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

func (store *InMemoryAllocationStore) poolKey(ns, name string) string {
	return ns + "/" + name
}

type AllocationSyncer interface {
	SetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, allocation *SandboxAllocation) error
	GetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*SandboxAllocation, error)
	GetRelease(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*AllocationRelease, error)
	SetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, released *AllocationReleased) error
	GetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*AllocationReleased, error)
}

type annoAllocationSyncer struct {
	client client.Client
}

func NewAnnoAllocationSyncer(client client.Client) AllocationSyncer {
	return &annoAllocationSyncer{
		client: client,
	}
}

func (syncer *annoAllocationSyncer) SetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, allocation *SandboxAllocation) error {
	js, err := json.Marshal(allocation)
	if err != nil {
		return err
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		anno = make(map[string]string)
	}
	anno[AnnoAllocStatusKey] = string(js)
	sandbox.SetAnnotations(anno)

	needAddFinalizer := !controllerutil.ContainsFinalizer(sandbox, FinalizerPoolAllocation)
	if needAddFinalizer {
		sandbox.SetFinalizers(append(sandbox.GetFinalizers(), FinalizerPoolAllocation))
	}

	meta := map[string]any{
		"annotations": map[string]string{
			AnnoAllocStatusKey: string(js),
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
	return syncer.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData))
}

func (syncer *annoAllocationSyncer) GetAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*SandboxAllocation, error) {
	allocation := &SandboxAllocation{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return allocation, nil
	}
	if raw := anno[AnnoAllocStatusKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), allocation)
		if err != nil {
			return nil, err
		}
	}
	return allocation, nil
}

func (syncer *annoAllocationSyncer) GetRelease(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*AllocationRelease, error) {
	release := &AllocationRelease{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return release, nil
	}
	if raw := anno[AnnoAllocReleaseKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), release)
		if err != nil {
			return nil, err
		}
	}
	return release, nil
}

func (syncer *annoAllocationSyncer) GetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*AllocationReleased, error) {
	released := &AllocationReleased{
		Pods: make([]string, 0),
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		return released, nil
	}
	if raw := anno[AnnoAllocReleasedKey]; raw != "" {
		err := json.Unmarshal([]byte(raw), released)
		if err != nil {
			return nil, err
		}
	}
	return released, nil
}

func (syncer *annoAllocationSyncer) SetReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, released *AllocationReleased) error {
	js, err := json.Marshal(released)
	if err != nil {
		return err
	}
	anno := sandbox.GetAnnotations()
	if anno == nil {
		anno = make(map[string]string)
	}
	anno[AnnoAllocReleasedKey] = string(js)
	sandbox.SetAnnotations(anno)

	needRemoveFinalizer := false
	// If the sandbox is being deleted and all allocated pods have been released,
	// remove the finalizer so the sandbox can be garbage collected.
	if !sandbox.DeletionTimestamp.IsZero() {
		allocation, err := syncer.GetAllocation(ctx, sandbox)
		if err != nil {
			return err
		}
		releasedSet := make(map[string]struct{}, len(released.Pods))
		for _, p := range released.Pods {
			releasedSet[p] = struct{}{}
		}
		allReleased := true
		for _, p := range allocation.Pods {
			if _, ok := releasedSet[p]; !ok {
				allReleased = false
				break
			}
		}
		if allReleased && controllerutil.ContainsFinalizer(sandbox, FinalizerPoolAllocation) {
			needRemoveFinalizer = true
			filtered := make([]string, 0, len(sandbox.GetFinalizers()))
			for _, f := range sandbox.GetFinalizers() {
				if f != FinalizerPoolAllocation {
					filtered = append(filtered, f)
				}
			}
			sandbox.SetFinalizers(filtered)
		}
	}

	meta := map[string]any{
		"annotations": map[string]string{
			AnnoAllocReleasedKey: string(js),
		},
	}
	if needRemoveFinalizer {
		meta["finalizers"] = sandbox.GetFinalizers()
	}
	patchData, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return err
	}
	obj := &sandboxv1alpha1.BatchSandbox{}
	obj.Name = sandbox.Name
	obj.Namespace = sandbox.Namespace
	return syncer.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData))
}

type AllocSpec struct {
	// Sandboxes contains all BatchSandboxes to be scheduled in this round.
	Sandboxes []*sandboxv1alpha1.BatchSandbox
	Pool      *sandboxv1alpha1.Pool
	// Pods contains all candidate pods owned by the pool.
	Pods []*corev1.Pod
}

type Allocator interface {
	Schedule(ctx context.Context, spec *AllocSpec) (*algorithm.AllocAction, error)
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
	store       AllocationStore
	syncer      AllocationSyncer
	client      client.Client
	algorithm   algorithm.Algorithm
	recoverOnce sync.Once
}

func NewDefaultAllocator(client client.Client) Allocator {
	return &defaultAllocator{
		store:     NewInMemoryAllocationStore(),
		syncer:    NewAnnoAllocationSyncer(client),
		client:    client,
		algorithm: &algorithm.PackedSchedule{},
	}
}

func (allocator *defaultAllocator) Schedule(ctx context.Context, spec *AllocSpec) (*algorithm.AllocAction, error) {
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

	// GC + per-sandbox allocation requests: build SandboxRequests for all existing sandboxes and
	// append orphan entries for pods whose sandbox no longer exists (e.g. force-deleted).
	// Orphan entries carry PodSupplement=0 and ToRelease=orphan pods so the normal recycle path
	// handles them without any special-casing outside this function.
	// Terminating sandboxes are handled inside getSandboxRequest: they receive no new supplement and
	// all unreleased pods are queued for release.
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
		request, err := allocator.getSandboxRequest(ctx, sandbox)
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

func (allocator *defaultAllocator) getSandboxRequest(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (*algorithm.SandboxRequest, error) {
	log := logf.FromContext(ctx)
	allocated, err := allocator.GetSandboxAllocation(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	released, err := allocator.GetSandboxReleased(ctx, sandbox)
	if err != nil {
		return nil, err
	}

	releasedSet := make(map[string]struct{}, len(released))
	for _, r := range released {
		releasedSet[r] = struct{}{}
	}

	// Terminating sandboxes should not receive new allocations.
	// Queue all unreleased allocated pods for release and set supplement to zero.
	if !sandbox.DeletionTimestamp.IsZero() {
		toRelease := make([]string, 0)
		for _, p := range allocated {
			if _, ok := releasedSet[p]; !ok {
				toRelease = append(toRelease, p)
			}
		}
		if len(toRelease) > 0 {
			log.Info("Queuing terminating sandbox pods for release", "sandbox", sandbox.Name, "pods", toRelease)
		}
		return &algorithm.SandboxRequest{
			SandboxName:   sandbox.Name,
			CurAllocation: allocated,
			CurReleased:   released,
			PodSupplement: 0,
			ToRelease:     toRelease,
		}, nil
	}

	release, err := allocator.getSandboxRelease(ctx, sandbox)
	if err != nil {
		return nil, err
	}

	toRelease := make([]string, 0)
	for _, r := range release {
		if _, exists := releasedSet[r]; !exists {
			toRelease = append(toRelease, r)
		}
	}

	replica := int32(0)
	if sandbox.Spec.Replicas != nil {
		replica = *sandbox.Spec.Replicas
	}

	supplement := int32(0)
	if replica-int32(len(allocated)) > 0 {
		supplement = replica - int32(len(allocated))
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
	return allocator.store.ClearAllocation(ctx, ns, poolName)
}

func (allocator *defaultAllocator) ReleasePodsAllocation(ctx context.Context, ns string, poolName string, pods []string) {
	allocator.store.ReleaseAllocation(ctx, ns, poolName, pods)
}

// SyncSandboxAllocation updates the in-memory allocation store and then persists to the sandbox annotation.
// If annotation sync fails, the in-memory store is rolled back to the previous state to maintain consistency.
func (allocator *defaultAllocator) SyncSandboxAllocation(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error {
	log := logf.FromContext(ctx)
	log.Info("Syncing sandbox allocation", "sandbox", sandbox.Name, "pods", pods)
	poolRef := sandbox.Spec.PoolRef

	// Snapshot the current in-memory state for rollback on failure.
	oldState, err := allocator.syncer.GetAllocation(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("failed to get current sandbox allocation: %w", err)
	}

	// Phase 1: update in-memory store optimistically.
	allocator.store.UpdateAllocation(ctx, sandbox.Namespace, poolRef, sandbox.Name, pods)

	// Phase 2: persist to sandbox annotation.
	allocation := &SandboxAllocation{Pods: pods}
	if err := allocator.syncer.SetAllocation(ctx, sandbox, allocation); err != nil {
		// Rollback in-memory store to the previous state.
		log.Error(err, "Rollback sandbox allocation", "sandbox", sandbox.Name, "pods", oldState.Pods)
		allocator.store.UpdateAllocation(ctx, sandbox.Namespace, poolRef, sandbox.Name, oldState.Pods)
		return err
	}
	return nil
}

// SyncSandboxReleased persists the released state to the sandbox annotation and then releases from the in-memory store.
// Annotation must succeed before the pods are removed from the in-memory store to prevent pods from being
// re-allocated before the release is durably committed.
func (allocator *defaultAllocator) SyncSandboxReleased(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, pods []string) error {
	log := logf.FromContext(ctx)
	log.Info("Syncing sandbox released", "sandbox", sandbox.Name, "pods", pods)
	poolRef := sandbox.Spec.PoolRef

	// Phase 1: persist to sandbox annotation.
	released := &AllocationReleased{Pods: pods}
	if err := allocator.syncer.SetReleased(ctx, sandbox, released); err != nil {
		log.Error(err, "Failed to sync sandbox released", "sandbox", sandbox.Name, "pods", pods)
		return err
	}

	// Phase 2: release from in-memory store only after the annotation is durably committed.
	allocator.store.ReleaseAllocation(ctx, sandbox.Namespace, poolRef, pods)
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
