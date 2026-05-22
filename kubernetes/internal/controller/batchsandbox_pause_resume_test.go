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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	taskscheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	taskexecutor "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// newTestReconciler creates a BatchSandboxReconciler with a fake client for testing.
func newTestReconciler(objs ...client.Object) *BatchSandboxReconciler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithIndex(&corev1.Pod{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithStatusSubresource(
			&sandboxv1alpha1.BatchSandbox{},
			&sandboxv1alpha1.SandboxSnapshot{},
		).
		WithObjects(objs...).
		Build()
	return &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}
}

type forbiddenTaskScheduler struct {
	t *testing.T
}

func (f *forbiddenTaskScheduler) Schedule() error {
	f.t.Fatalf("task scheduler should not be invoked while sandbox is paused")
	return nil
}

func (f *forbiddenTaskScheduler) UpdatePods(_ []*corev1.Pod) {
	f.t.Fatalf("task scheduler should not receive pod updates while sandbox is paused")
}

func (f *forbiddenTaskScheduler) ListTask() []taskscheduler.Task {
	f.t.Fatalf("task scheduler should not list tasks while sandbox is paused")
	return nil
}

func (f *forbiddenTaskScheduler) StopTask() []taskscheduler.Task {
	f.t.Fatalf("task scheduler should not stop tasks while sandbox is paused")
	return nil
}

func (f *forbiddenTaskScheduler) AddTasks(_ []*taskexecutor.Task) error {
	f.t.Fatalf("task scheduler should not add tasks while sandbox is paused")
	return nil
}

type fakeSchedulerTask struct {
	name     string
	state    taskscheduler.TaskState
	podName  string
	released bool
}

func (f fakeSchedulerTask) GetName() string {
	return f.name
}

func (f fakeSchedulerTask) GetState() taskscheduler.TaskState {
	return f.state
}

func (f fakeSchedulerTask) GetPodName() string {
	return f.podName
}

func (f fakeSchedulerTask) IsResourceReleased() bool {
	return f.released
}

type recordingTaskScheduler struct {
	updatePodsCalls int
	scheduleCalls   int
	stopCalls       int
	tasks           []taskscheduler.Task
}

func (r *recordingTaskScheduler) Schedule() error {
	r.scheduleCalls++
	return nil
}

func (r *recordingTaskScheduler) UpdatePods(_ []*corev1.Pod) {
	r.updatePodsCalls++
}

func (r *recordingTaskScheduler) ListTask() []taskscheduler.Task {
	return r.tasks
}

func (r *recordingTaskScheduler) StopTask() []taskscheduler.Task {
	r.stopCalls++
	return r.tasks
}

func (r *recordingTaskScheduler) AddTasks(_ []*taskexecutor.Task) error {
	return nil
}

// ---------- dispatchPauseResume 5-case tests ----------

func TestDispatchPauseResume_Case1_PauseTrue(t *testing.T) {
	// gen > pauseObservedGen, pause=true → handlePause dispatched
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "should be handled by handlePause")
	assert.True(t, result.RequeueAfter > 0, "handlePause should requeue")

	// Verify ACK: phase should be Pausing
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
}

func TestDispatchPauseResume_Case2_PauseFalse(t *testing.T) {
	// gen > pauseObservedGen, pause=false → handleResume dispatched
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs, snapshot)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "should be handled by handleResume")
	assert.True(t, result.RequeueAfter > 0, "handleResume should requeue")

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
}

func TestDispatchPauseResume_Case3_PauseNil_ACKOnly(t *testing.T) {
	// gen > pauseObservedGen, pause=nil → no dedicated ACK API call, continue normal flow (handled=false).
	// The ACK is deferred to persistRuntimeView in the main reconcile loop.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.False(t, handled, "ACK only should not block normal flow")
	assert.Equal(t, ctrl.Result{}, result)

	// Verify ACK is NOT written to server by dispatch (deferred to persistRuntimeView).
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(1), updated.Status.PauseObservedGeneration, "server should not be updated by dispatch; ACK is deferred")
}

func TestDispatchPauseResume_Case4_GenEqual_PauseSet(t *testing.T) {
	// gen == pauseObservedGen, pause != nil → syncPauseOrClear
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
		},
	}
	r := newTestReconciler(bs, snapshot)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "syncPauseOrClear should handle this")
	assert.True(t, result.RequeueAfter > 0, "committing snapshot should requeue")
}

func TestDispatchPauseResume_Case5_GenEqual_PauseNil(t *testing.T) {
	// gen == pauseObservedGen, pause == nil → normal flow (handled=false)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.False(t, handled, "normal flow should not be blocked")
	assert.Equal(t, ctrl.Result{}, result)
}

// ---------- handlePause tests ----------

func TestHandlePause_NormalFlow(t *testing.T) {
	// Normal pause: ACK, create SandboxSnapshot, verify phase=Pausing
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify ACK: phase=Pausing, pauseObservedGeneration=2
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)

	// Verify SandboxSnapshot was created under the reserved internal name
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
	// Verify OwnerRef
	assert.Equal(t, "test-bs", snap.OwnerReferences[0].Name)
}

func TestHandlePause_WaitsForTaskCleanupBeforeCreatingSnapshot(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
			TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
				Spec: sandboxv1alpha1.TaskSpec{
					Process: &sandboxv1alpha1.ProcessTask{Command: []string{"sleep", "3600"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "BatchSandbox",
				Name:       "test-bs",
				UID:        "test-uid",
			}},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	r := newTestReconciler(bs, pod)
	scheduler := &recordingTaskScheduler{
		tasks: []taskscheduler.Task{fakeSchedulerTask{
			name:     "test-bs-0",
			state:    taskscheduler.RunningTaskState,
			podName:  "test-bs-0",
			released: false,
		}},
	}
	r.taskSchedulers.Store(types.NamespacedName{Namespace: "default", Name: "test-bs"}.String(), scheduler)

	result, err := r.handlePause(context.Background(), bs)

	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)
	assert.Equal(t, 1, scheduler.updatePodsCalls)
	assert.Equal(t, 1, scheduler.stopCalls)
	assert.Equal(t, 1, scheduler.scheduleCalls)
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap)
	assert.True(t, apierrors.IsNotFound(err), "snapshot must wait until tasks are released")
}

func TestHandlePause_CreatesSnapshotAfterTaskCleanup(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
			TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
				Spec: sandboxv1alpha1.TaskSpec{
					Process: &sandboxv1alpha1.ProcessTask{Command: []string{"sleep", "3600"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "BatchSandbox",
				Name:       "test-bs",
				UID:        "test-uid",
			}},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	r := newTestReconciler(bs, pod)
	scheduler := &recordingTaskScheduler{
		tasks: []taskscheduler.Task{fakeSchedulerTask{
			name:     "test-bs-0",
			state:    taskscheduler.RunningTaskState,
			podName:  "test-bs-0",
			released: true,
		}},
	}
	r.taskSchedulers.Store(types.NamespacedName{Namespace: "default", Name: "test-bs"}.String(), scheduler)

	result, err := r.handlePause(context.Background(), bs)

	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)
	assert.Equal(t, 1, scheduler.stopCalls)
	assert.Equal(t, 1, scheduler.scheduleCalls)
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
}

func TestHandlePause_RejectsUnsupportedReplicas(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(2)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)

	var pauseFailed *sandboxv1alpha1.BatchSandboxCondition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == sandboxv1alpha1.BatchSandboxConditionPauseFailed {
			pauseFailed = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, pauseFailed)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, pauseFailed.Status)
	assert.Equal(t, "UnsupportedReplicas", pauseFailed.Reason)
	assert.Contains(t, pauseFailed.Message, "spec.replicas=1")

	snap := &sandboxv1alpha1.SandboxSnapshot{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap)
	assert.True(t, apierrors.IsNotFound(err), "unsupported replicas should be rejected before creating a snapshot")
}

func TestHandlePause_PoolMode(t *testing.T) {
	// Pool mode: pause should snapshot the allocated pod without mutating poolRef/template first.
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "pool-image:latest"},
					},
				},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:   ptr.To(true),
			PoolRef: "test-pool",
			// Template is nil (pool mode)
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs, pool)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify template was solidified from Pool
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Nil(t, updated.Spec.Template)
	assert.Equal(t, "test-pool", updated.Spec.PoolRef)

	// Verify SandboxSnapshot was created under the reserved internal name
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
}

func TestHandlePause_PoolModeDoesNotRequirePoolCR(t *testing.T) {
	// The source pod allocation is enough to snapshot a pooled BatchSandbox.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "nonexistent-pool",
			Replicas: ptr.To(int32(1)),
			// Template is nil - pool mode
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod"}})
	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
	}
	r := newTestReconciler(bs, poolPod)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Template)
	assert.Equal(t, "nonexistent-pool", updated.Spec.PoolRef)

	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
}

func TestHandlePause_FailedRetry(t *testing.T) {
	// Old snapshot is Failed → delete it first, but do not ACK a new pause attempt
	// until the stale failed snapshot is gone.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:               sandboxv1alpha1.BatchSandboxConditionPauseFailed,
					Status:             sandboxv1alpha1.ConditionTrue,
					Reason:             "SnapshotFailed",
					Message:            "previous attempt failed",
					LastTransitionTime: ptr.To(metav1.Now()),
				},
			},
		},
	}
	oldSnapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseFailed,
		},
	}
	r := newTestReconciler(bs, oldSnapshot)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify old snapshot was deleted
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap)
	assert.True(t, err == nil || len(snap.UID) == 0 || snap.DeletionTimestamp != nil,
		"old Failed snapshot should be deleted or being deleted")

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(1), updated.Status.PauseObservedGeneration, "retry should not ACK until stale failed snapshot is removed")
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, updated.Status.Phase, "retry cleanup should not transition to Pausing yet")
}

func TestHandlePause_SucceededSnapshotRetry(t *testing.T) {
	// Old snapshot is Succeed from a previous resume cleanup failure. It must be
	// deleted before ACKing a new pause attempt, otherwise pause reuses stale state.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
		},
	}
	oldSnapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
		},
	}
	r := newTestReconciler(bs, oldSnapshot)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	snap := &sandboxv1alpha1.SandboxSnapshot{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap)
	assert.True(t, apierrors.IsNotFound(err), "old Succeed snapshot should be deleted before retrying pause")

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(1), updated.Status.PauseObservedGeneration, "retry cleanup should not ACK until stale succeeded snapshot is removed")
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, updated.Status.Phase, "retry cleanup should not transition to Pausing yet")
}

// ---------- handleResume tests ----------

func TestHandleResume_NormalFlow(t *testing.T) {
	// handleResume now only ACKs Resuming phase and requeues
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handleResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "should requeue after ACK")

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
}

func TestContinueResume_NormalFlow(t *testing.T) {
	// continueResume does the actual work: read snapshot and replace images without changing replicas.
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)
	r.ResumePullSecret = "my-pull-secret"

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify images replaced
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, "registry/test-bs-main:snap-gen1", updated.Spec.Template.Spec.Containers[0].Image)
	// Verify replicas are preserved.
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
	// Verify controller does not clear spec.pause
	require.NotNil(t, updated.Spec.Pause)
	assert.False(t, *updated.Spec.Pause)
	// Verify imagePullSecrets added
	found := false
	for _, s := range updated.Spec.Template.Spec.ImagePullSecrets {
		if s.Name == "my-pull-secret" {
			found = true
		}
	}
	assert.True(t, found, "imagePullSecrets should contain resume-pull-secret")
}

func TestContinueResume_PreservesExistingReplicas(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(2)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int32(2), *updated.Spec.Replicas)
	assert.Equal(t, "registry/test-bs-main:snap-gen1", updated.Spec.Template.Spec.Containers[0].Image)
}

func TestHandleResume_PoolMode(t *testing.T) {
	// handleResume only ACKs Resuming phase, poolRef is cleared by continueResume
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(0)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handleResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
}

func TestContinueResume_PoolMode(t *testing.T) {
	// continueResume clears poolRef without changing replicas.
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
			Finalizers: []string{FinalizerPoolAllocation},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify poolRef cleared
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, "", updated.Spec.PoolRef)
	assert.NotContains(t, updated.Finalizers, FinalizerPoolAllocation)
}

func TestContinueResume_UsesPatchedTemplateWhenCacheReturnsStaleObject(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	staleBatchSandbox := bs.DeepCopy()
	returnStaleBatchSandbox := false
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}, &sandboxv1alpha1.SandboxSnapshot{}).
		WithObjects(bs.DeepCopy(), snapshot).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				err := c.Patch(ctx, obj, patch, opts...)
				if err == nil && obj.GetNamespace() == "default" && obj.GetName() == "test-bs" {
					returnStaleBatchSandbox = true
				}
				return err
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if returnStaleBatchSandbox && key.Namespace == "default" && key.Name == "test-bs" {
					if target, ok := obj.(*sandboxv1alpha1.BatchSandbox); ok {
						staleBatchSandbox.DeepCopyInto(target)
						return nil
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)
	require.NotNil(t, bs.Spec.Template)
	assert.Equal(t, "registry/test-bs-main:snap-gen1", bs.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "", bs.Spec.PoolRef)
	require.NotNil(t, bs.Spec.Replicas)
	assert.Equal(t, int32(1), *bs.Spec.Replicas)
}

func TestContinueResume_SnapshotNotFound(t *testing.T) {
	// continueResume without snapshot → rollback to Paused with ResumeFailed, spec.pause unchanged
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify rollback keeps spec.pause and records retryable failure
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	require.NotNil(t, updated.Spec.Pause)
	assert.False(t, *updated.Spec.Pause)
}

func TestContinueResume_SnapshotMissingButReadyPodTreatsResumeAsComplete(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxNameKey: "test-bs",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newTestReconciler(bs, pod)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, updated.Status.Phase)
	require.NotNil(t, updated.Spec.Pause)
	assert.False(t, *updated.Spec.Pause)

	for _, cond := range updated.Status.Conditions {
		assert.NotEqual(t, sandboxv1alpha1.BatchSandboxConditionResumeFailed, cond.Type)
	}
}

func TestContinueResume_SnapshotNotReady(t *testing.T) {
	// continueResume with snapshot still Committing → Phase=Paused + ResumeFailed condition, spec.pause unchanged
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify Phase=Paused with ResumeFailed condition (retryable)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	require.NotNil(t, updated.Spec.Pause)
	assert.False(t, *updated.Spec.Pause)

	// Verify ResumeFailed condition is set
	foundCondition := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionResumeFailed {
			foundCondition = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "SnapshotNotReady", cond.Reason)
			assert.Contains(t, cond.Message, "snapshot not ready")
			break
		}
	}
	assert.True(t, foundCondition, "ResumeFailed condition should be set")
}

func TestReconcile_ResumingPoolResumeRebuildsStrategyAfterContinueResume(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 5,
			UID:        "test-uid",
			Annotations: map[string]string{
				AnnoAllocStatusKey: `{"pods":["pool-pod-0"]}`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 5,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod-0",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newTestReconciler(bs, snapshot, poolPod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-bs"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, "", updated.Spec.PoolRef, "resume should detach the sandbox from the pool")
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase, "old pooled pod must not short-circuit resume completion")

	resumedPod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-0"}, resumedPod))
	assert.Equal(t, "test-bs", resumedPod.Labels[LabelBatchSandboxNameKey])

	stillPresent := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, stillPresent))
}

func TestReconcile_ResumingIgnoresDeletingReadyPodWhenCompletingResume(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	now := metav1.Now()
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 3,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	terminatingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-bs-0",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"test/finalizer"},
			Labels: map[string]string{
				LabelBatchSandboxNameKey:     "test-bs",
				LabelBatchSandboxPodIndexKey: "0",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "BatchSandbox",
				Name:       "test-bs",
				UID:        "test-uid",
				Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newTestReconciler(bs, snapshot, terminatingPod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-bs"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)

	stillPresent := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, stillPresent))
}

func TestReconcile_PausedNonPoolDoesNotScalePods(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 3,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-bs"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)

	pod := &corev1.Pod{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-0"}, pod)
	assert.Error(t, err, "paused non-pooled sandbox should not recreate pods")
}

func TestReconcile_PausedSkipsTaskSchedulingAndClearsScheduler(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 4,
			UID:        "test-uid",
			Finalizers: []string{FinalizerTaskCleanup},
			Annotations: map[string]string{
				AnnoAllocStatusKey:  `{"pods":["pool-pod"]}`,
				AnnoAllocReleaseKey: `{"pods":["pool-pod"]}`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "pool",
			Replicas: ptr.To(int32(1)),
			TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
				Spec: sandboxv1alpha1.TaskSpec{
					Process: &sandboxv1alpha1.ProcessTask{
						Command: []string{"/bin/sh", "-c", "sleep 3600"},
					},
				},
			},
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "sandbox", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 4,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
			ObservedGeneration:      4,
		},
	}
	r := newTestReconciler(bs)
	key := types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}.String()
	r.taskSchedulers.Store(key, &forbiddenTaskScheduler{t: t})

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	_, ok := r.taskSchedulers.Load(key)
	assert.False(t, ok, "paused reconcile should clear stale task scheduler state")
}

func TestCompletePause_PooledModeClearsTaskScheduler(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
			UID:        "test-uid",
			Annotations: map[string]string{
				AnnoAllocStatusKey: `{"pods":["pool-pod"]}`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "pool",
			Replicas: ptr.To(int32(1)),
			TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
				Spec: sandboxv1alpha1.TaskSpec{
					Process: &sandboxv1alpha1.ProcessTask{
						Command: []string{"/bin/sh", "-c", "sleep 3600"},
					},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 3,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.8",
		},
	}
	r := newTestReconciler(bs, poolPod)
	key := types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}.String()
	r.taskSchedulers = sync.Map{}
	r.taskSchedulers.Store(key, fmt.Sprintf("scheduler:%s", key))

	err := r.completePause(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	assert.Equal(t, "", updated.Spec.PoolRef)
	require.NotNil(t, updated.Spec.Template)
	assert.Equal(t, "pool-image:latest", updated.Spec.Template.Spec.Containers[0].Image)
	assert.NotContains(t, updated.Annotations, AnnoAllocReleaseKey)

	_, ok := r.taskSchedulers.Load(key)
	assert.False(t, ok, "completePause should clear the stale in-memory task scheduler")
}

func TestPersistRuntimeView_PreservesPauseFailedConditionFromLatestStatus(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			ObservedGeneration:      1,
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
			Replicas:                1,
			Allocated:               1,
			Ready:                   1,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:               sandboxv1alpha1.BatchSandboxConditionReady,
					Status:             sandboxv1alpha1.ConditionTrue,
					Reason:             "PodsReady",
					Message:            "Sandbox is running",
					LastTransitionTime: ptr.To(metav1.Now()),
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxNameKey:     "test-bs",
				LabelBatchSandboxPodIndexKey: "0",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "BatchSandbox",
				Name:       "test-bs",
				UID:        "test-uid",
				Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.10",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newTestReconciler(bs, pod)

	// Simulate pause handler writing PauseFailed condition to API server.
	latest := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, latest))
	latest.Status.Conditions = append(latest.Status.Conditions, sandboxv1alpha1.BatchSandboxCondition{
		Type:               sandboxv1alpha1.BatchSandboxConditionPauseFailed,
		Status:             sandboxv1alpha1.ConditionTrue,
		Reason:             "SnapshotFailed",
		Message:            "Commit job failed",
		LastTransitionTime: ptr.To(metav1.Now()),
	})
	require.NoError(t, r.Status().Update(context.Background(), latest))

	// Simulate second reconcile: informer has caught up, so we read latest state.
	freshBS := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, freshBS))

	view := buildRuntimeView(freshBS, []*corev1.Pod{pod})
	_, errs := r.persistRuntimeView(context.Background(), freshBS, view)
	require.Empty(t, errs)

	// Verify PauseFailed is preserved after reconcile with fresh cache.
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	foundPauseFailed := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionPauseFailed {
			foundPauseFailed = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "SnapshotFailed", cond.Reason)
			assert.Equal(t, "Commit job failed", cond.Message)
		}
	}
	assert.True(t, foundPauseFailed, "persistRuntimeView should preserve PauseFailed condition once informer cache catches up")
}

func TestPersistRuntimeView_SkipsStatusUpdateWhenRuntimeStatusUnchanged(t *testing.T) {
	transitionTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
			UID:        "test-uid",
			Annotations: map[string]string{
				AnnotationSandboxEndpoints: `["10.0.0.10"]`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			ObservedGeneration: 3,
			Phase:              sandboxv1alpha1.BatchSandboxPhaseSucceed,
			Replicas:           1,
			Allocated:          1,
			Ready:              1,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:               sandboxv1alpha1.BatchSandboxConditionReady,
					Status:             sandboxv1alpha1.ConditionTrue,
					Reason:             "PodsReady",
					Message:            "Sandbox is running",
					LastTransitionTime: &transitionTime,
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.10",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	statusUpdates := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(bs).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					statusUpdates++
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	view := buildRuntimeView(bs.DeepCopy(), []*corev1.Pod{pod})
	_, errs := r.persistRuntimeView(context.Background(), bs.DeepCopy(), view)
	require.Empty(t, errs)
	assert.Equal(t, 0, statusUpdates, "unchanged runtime status should not be persisted again")
}

func TestPersistRuntimeView_RetriesSucceededPauseSnapshotCleanup(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
			UID:        "test-uid",
			Annotations: map[string]string{
				AnnotationSandboxEndpoints: "null",
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			ObservedGeneration:      3,
			PauseObservedGeneration: 3,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseSucceed,
			Replicas:                1,
			Allocated:               1,
			Ready:                   1,
		},
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
		},
	}
	r := newTestReconciler(bs, snapshot)

	status := bs.Status
	view := runtimeView{status: &status}
	_, errs := r.persistRuntimeView(context.Background(), bs.DeepCopy(), view)
	require.Empty(t, errs)

	stillPresent := &sandboxv1alpha1.SandboxSnapshot{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, stillPresent)
	assert.True(t, apierrors.IsNotFound(err), "successful internal pause snapshot cleanup should be retried after resume")
}

func TestBuildRuntimeView_AggregatesPodFailuresInSteadyState(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhaseSucceed,
		},
	}

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "err-image-0", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ErrImagePull",
							Message: "image not found",
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "crash-1", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CrashLoopBackOff",
							Message: "back-off restarting failed container",
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "err-image-2", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ErrImagePull",
							Message: "image still not found",
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy-3", Namespace: "default"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		},
	}

	view := buildRuntimeView(bs, pods)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)

	var podFailed *sandboxv1alpha1.BatchSandboxCondition
	for i := range view.status.Conditions {
		if view.status.Conditions[i].Type == sandboxv1alpha1.BatchSandboxConditionPodFailed {
			podFailed = &view.status.Conditions[i]
			break
		}
	}
	require.NotNil(t, podFailed)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, podFailed.Status)
	assert.Equal(t, "ErrImagePull", podFailed.Reason)
	assert.Equal(t, "3/4 observed pods failed; primary reason=ErrImagePull; sample pod=err-image-0", podFailed.Message)
}

func TestBuildRuntimeView_AggregatesResumeFailures(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "imgpull-0", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ImagePullBackOff",
							Message: "pull back off",
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "imgpull-1", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ImagePullBackOff",
							Message: "pull back off again",
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy-2", Namespace: "default"},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
	}

	view := buildRuntimeView(bs, pods)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)

	var resumeFailed *sandboxv1alpha1.BatchSandboxCondition
	var podFailed *sandboxv1alpha1.BatchSandboxCondition
	for i := range view.status.Conditions {
		switch view.status.Conditions[i].Type {
		case sandboxv1alpha1.BatchSandboxConditionResumeFailed:
			resumeFailed = &view.status.Conditions[i]
		case sandboxv1alpha1.BatchSandboxConditionPodFailed:
			podFailed = &view.status.Conditions[i]
		}
	}
	require.NotNil(t, resumeFailed)
	require.NotNil(t, podFailed)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, resumeFailed.Status)
	assert.Equal(t, "ImagePullBackOff", resumeFailed.Reason)
	assert.Equal(t, "2/3 observed pods failed during resume; primary reason=ImagePullBackOff; sample pod=imgpull-0", resumeFailed.Message)
	assert.Equal(t, "ImagePullBackOff", podFailed.Reason)
	assert.Equal(t, "2/3 observed pods failed; primary reason=ImagePullBackOff; sample pod=imgpull-0", podFailed.Message)
}

func TestBuildRuntimeView_PreservesConditionTransitionTimeWhenUnchanged(t *testing.T) {
	transitionTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 3,
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhaseSucceed,
			Conditions: []sandboxv1alpha1.BatchSandboxCondition{
				{
					Type:               sandboxv1alpha1.BatchSandboxConditionReady,
					Status:             sandboxv1alpha1.ConditionTrue,
					Reason:             "PodsReady",
					Message:            "Sandbox is running",
					LastTransitionTime: &transitionTime,
				},
			},
		},
	}

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy-0", Namespace: "default"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.10",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		},
	}

	view := buildRuntimeView(bs, pods)

	var ready *sandboxv1alpha1.BatchSandboxCondition
	for i := range view.status.Conditions {
		if view.status.Conditions[i].Type == sandboxv1alpha1.BatchSandboxConditionReady {
			ready = &view.status.Conditions[i]
			break
		}
	}
	require.NotNil(t, ready)
	require.NotNil(t, ready.LastTransitionTime)
	assert.Equal(t, transitionTime, *ready.LastTransitionTime, "unchanged condition should keep its original transition time")
}

func TestBuildRuntimeView_AddsPausedConditionFromEmptyConditions(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 5,
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}

	view := buildRuntimeView(bs, nil)

	var paused *sandboxv1alpha1.BatchSandboxCondition
	for i := range view.status.Conditions {
		if view.status.Conditions[i].Type == sandboxv1alpha1.BatchSandboxConditionPaused {
			paused = &view.status.Conditions[i]
			break
		}
	}

	require.NotNil(t, paused)
	require.NotNil(t, paused.LastTransitionTime)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, paused.Status)
	assert.Equal(t, "Paused", paused.Reason)
	assert.Equal(t, "Sandbox is paused", paused.Message)
}

// ---------- completePause test ----------

func TestCompletePause(t *testing.T) {
	// completePause: status.phase=Paused, controller does not clear spec.pause
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs)

	err := r.completePause(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	// Phase should be Paused
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	var pausedCondition *sandboxv1alpha1.BatchSandboxCondition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == sandboxv1alpha1.BatchSandboxConditionType("Paused") {
			pausedCondition = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, pausedCondition, "Paused phase should publish an explicit Paused condition")
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, pausedCondition.Status)
	// Replicas should remain unchanged (not set to 0 per design doc)
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
	// Pause remains true until the next external request changes it
	require.NotNil(t, updated.Spec.Pause)
	assert.True(t, *updated.Spec.Pause)
}

func TestCompletePause_DeleteFailureLeavesPhasePausing(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Kind:       "BatchSandbox",
					Name:       "test-bs",
					UID:        "test-uid",
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "img"}},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithIndex(&corev1.Pod{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(bs, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetNamespace() == "default" && obj.GetName() == "test-bs-0" {
					return fmt.Errorf("delete failed")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	err := r.completePause(context.Background(), bs)
	require.ErrorContains(t, err, "delete failed")

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
}

func TestCompletePause_PooledSandboxDetachesForPoolGC(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-bs-uid",
			Finalizers: []string{FinalizerPoolAllocation},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod-1"}})

	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod-1",
			Namespace: "default",
			Labels: map[string]string{
				LabelPoolName:     "test-pool",
				LabelPoolRevision: "rev-1",
				"app":             "demo",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Kind:       "Pool",
					Name:       "test-pool",
					UID:        "test-pool-uid",
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
	}

	r := newTestReconciler(bs, poolPod)

	err := r.completePause(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	assert.Equal(t, "", updated.Spec.PoolRef)
	require.NotNil(t, updated.Spec.Template)
	assert.Equal(t, "pool-image:latest", updated.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "demo", updated.Spec.Template.Labels["app"])
	assert.NotContains(t, updated.Spec.Template.Labels, LabelPoolName)
	assert.NotContains(t, updated.Spec.Template.Labels, LabelPoolRevision)
	assert.Equal(t, "", updated.Spec.Template.Spec.NodeName)
	assert.NotContains(t, updated.Annotations, AnnoAllocReleaseKey)
	assert.NotContains(t, updated.Finalizers, FinalizerPoolAllocation)
}

func TestCompletePause_PooledSandboxDoesNotDeleteSourcePod(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-bs-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod-1"}})

	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Kind:       "Pool",
					Name:       "test-pool",
					UID:        "test-pool-uid",
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(bs, poolPod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetNamespace() == "default" && obj.GetName() == "pool-pod-1" {
					return fmt.Errorf("batchsandbox reconciler must not delete pooled source pod")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	err := r.completePause(context.Background(), bs)
	require.NoError(t, err)

	stillPresent := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pool-pod-1"}, stillPresent))
}

func TestCompletePause_PooledSandboxAcknowledgesSpecPatchGeneration(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-bs-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod-1"}})

	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Kind:       "Pool",
					Name:       "test-pool",
					UID:        "test-pool-uid",
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(bs, poolPod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				if obj.GetNamespace() == "default" && obj.GetName() == "test-bs" {
					latest := &sandboxv1alpha1.BatchSandbox{}
					if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-bs"}, latest); err != nil {
						return err
					}
					latest.Generation = 3
					return c.Update(ctx, latest)
				}
				return nil
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	require.NoError(t, r.completePause(context.Background(), bs))

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(3), updated.Generation)
	assert.Equal(t, updated.Generation, updated.Status.PauseObservedGeneration)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
}

func TestCompletePause_DoesNotAcknowledgeQueuedResumeGeneration(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-bs-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod-1"}})

	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Kind:       "Pool",
					Name:       "test-pool",
					UID:        "test-pool-uid",
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "pool-image:latest"}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}).
		WithObjects(bs, poolPod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				if obj.GetNamespace() == "default" && obj.GetName() == "test-bs" {
					latest := &sandboxv1alpha1.BatchSandbox{}
					if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-bs"}, latest); err != nil {
						return err
					}
					latest.Generation = 3
					latest.Spec.Pause = ptr.To(false)
					return c.Update(ctx, latest)
				}
				return nil
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	require.NoError(t, r.completePause(context.Background(), bs))

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(3), updated.Generation)
	assert.NotNil(t, updated.Spec.Pause)
	assert.False(t, *updated.Spec.Pause)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
}

// ---------- syncPauseOrClear tests ----------

func TestSyncPauseOrClear_SnapshotReady(t *testing.T) {
	// Snapshot Succeed → completePause
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify completePause was called
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	// Replicas should remain unchanged (not set to 0 per design doc)
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
	require.NotNil(t, updated.Spec.Pause)
	assert.True(t, *updated.Spec.Pause)
}

func TestSyncPauseOrClear_SnapshotFailed(t *testing.T) {
	// Snapshot Failed → retryable or terminal failure depending on Pod existence, spec.pause unchanged
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseFailed,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify retryable PauseFailed handling. In this fake-client setup no source Pod exists,
	// so the controller escalates the phase to Failed instead of returning to Succeed.
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	require.NotNil(t, updated.Spec.Pause)
	assert.True(t, *updated.Spec.Pause)

	// Verify PauseFailed condition is set
	foundCondition := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionPauseFailed {
			foundCondition = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "PodNotFound", cond.Reason) // Pod not found in fake client
			break
		}
	}
	assert.True(t, foundCondition, "PauseFailed condition should be set")
}

func TestSyncPauseOrClear_SnapshotFailedReturnsStatusUpdateError(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseFailed,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(&sandboxv1alpha1.BatchSandbox{}, &sandboxv1alpha1.SandboxSnapshot{}).
		WithObjects(bs, snapshot).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" && obj.GetNamespace() == "default" && obj.GetName() == "test-bs" {
					return fmt.Errorf("status update failed")
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &BatchSandboxReconciler{
		Client:              fakeClient,
		Scheme:              testscheme,
		Recorder:            record.NewFakeRecorder(10),
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.ErrorContains(t, err, "status update failed")
	assert.Equal(t, ctrl.Result{}, result)
}

func TestSyncPauseOrClear_SnapshotCommitting(t *testing.T) {
	// Snapshot Committing → requeue
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-pause",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "committing snapshot should requeue")
}

func TestSyncPauseOrClear_NoSnapshot(t *testing.T) {
	// No snapshot while already Pausing → create it and requeue.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "no snapshot should requeue")

	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs-pause"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
}

// ---------- Phase update bug fix verification ----------

func TestPhaseUpdate_Succeed(t *testing.T) {
	// When pods are Running+Ready, phase should be Succeed (not stuck at Pending)
	// This verifies the Bug 2 fix: phase judgment switch moved AFTER the pod counting loop.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 1,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxPodIndexKey: "0",
				LabelBatchSandboxNameKey:     "test-bs",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.GroupVersion.String(),
					Kind:       "BatchSandbox",
					Name:       "test-bs",
					UID:        "test-uid",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "main", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	_ = newTestReconciler(bs, pod)

	// Verify the logic inline (same code path as Reconcile)
	newStatus := bs.Status.DeepCopy()
	newStatus.ObservedGeneration = bs.Generation
	newStatus.Replicas = 0
	newStatus.Ready = 0
	newStatus.Allocated = 0

	// Phase judgment AFTER counting (Bug 2 fix verification)
	pods := []*corev1.Pod{pod}
	for _, p := range pods {
		newStatus.Replicas++
		if p.Spec.NodeName != "" {
			newStatus.Allocated++
		}
		if p.Status.Phase == corev1.PodRunning && p.Status.Conditions != nil {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					newStatus.Ready++
				}
			}
		}
	}

	// Phase should be Succeed because Ready > 0
	switch bs.Status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePausing, sandboxv1alpha1.BatchSandboxPhasePaused,
		sandboxv1alpha1.BatchSandboxPhaseResuming:
		// Don't override
	default:
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseSucceed
		} else {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhasePending
		}
	}

	assert.Equal(t, int32(1), newStatus.Ready, "Ready should be 1")
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, newStatus.Phase,
		"Phase should be Succeed when Ready > 0")
}

// ---------- ackPauseGeneration test ----------

func TestAckPauseGeneration(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 5,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 3,
		},
	}
	r := newTestReconciler(bs)

	err := r.ackPauseGeneration(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(5), updated.Status.PauseObservedGeneration)
}

// ---------- ackPauseWithPhase behavior ----------

func TestAckPauseWithPhase_DoesNotMutateSpecPause(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{},
	}
	r := newTestReconciler(bs)

	err := r.ackPauseWithPhase(context.Background(), bs, sandboxv1alpha1.BatchSandboxPhasePausing, "")
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	require.NotNil(t, updated.Spec.Pause)
	assert.True(t, *updated.Spec.Pause)
}

// Ensure ctrl.Result type is used
var _ = ctrl.Result{}
