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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func recoveredRuntimePod(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test"}, {Name: "sidecar", Image: "test"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			// Deliberately put the sidecar first: match the main container by name.
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
}

func TestPodFailureRecovery(t *testing.T) {
	for _, phase := range []sandboxv1alpha1.BatchSandboxPhase{"", sandboxv1alpha1.BatchSandboxPhasePending, sandboxv1alpha1.BatchSandboxPhaseSucceed} {
		t.Run(string(phase), func(t *testing.T) {
			bs := &sandboxv1alpha1.BatchSandbox{Status: sandboxv1alpha1.BatchSandboxStatus{Phase: phase, TaskFailed: 2}}
			pod := recoveredRuntimePod("sandbox-0", "original")
			failed := pod.DeepCopy()
			failed.Status.Conditions = nil
			if phase == sandboxv1alpha1.BatchSandboxPhaseSucceed {
				failed.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			} else {
				failed.Status.Phase = corev1.PodFailed
				failed.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
			}
			view := buildRuntimeView(bs, []*corev1.Pod{failed})
			require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)
			require.Equal(t, []string{"original"}, view.status.FailedPodUIDs)
			bs.Status = *view.status
			view = buildRuntimeView(bs, []*corev1.Pod{pod})
			assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, view.status.Phase)
			assert.Empty(t, view.status.FailedPodUIDs)
			assert.False(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
			assert.True(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionReady))
			assert.Equal(t, int32(2), view.status.TaskFailed)
			bs.Status = *view.status
			assert.Equal(t, bs.Status, *buildRuntimeView(bs, []*corev1.Pod{pod}).status, "recovery must be idempotent")
		})
	}
}

func TestPodFailureRecoveryRequiresAllOriginalPods(t *testing.T) {
	cases := map[string]func(*sandboxv1alpha1.BatchSandbox, *[]*corev1.Pod){
		"replacement": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) { (*pods)[1].UID = "replacement" },
		"missing":     func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) { *pods = (*pods)[:1] },
		"deleting": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			now := metav1.Now()
			(*pods)[1].DeletionTimestamp = &now
		},
		"not ready": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) { (*pods)[1].Status.Conditions = nil },
		"not running": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			(*pods)[1].Status.Phase = corev1.PodPending
		},
		"main terminated": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			(*pods)[1].Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
		},
		"main waiting": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			(*pods)[1].Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		},
		"no main status": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			(*pods)[1].Status.ContainerStatuses = (*pods)[1].Status.ContainerStatuses[:1]
		},
		"no containers": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) { (*pods)[1].Spec.Containers = nil },
		"no provenance": func(bs *sandboxv1alpha1.BatchSandbox, _ *[]*corev1.Pod) { bs.Status.FailedPodUIDs = nil },
		"empty UID": func(bs *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			bs.Status.FailedPodUIDs[1] = ""
			(*pods)[1].UID = ""
		},
		"resume failed": func(bs *sandboxv1alpha1.BatchSandbox, _ *[]*corev1.Pod) {
			setConditionInStatus(&bs.Status, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "ResumeFailed", "failed")
		},
		"another failing pod": func(_ *sandboxv1alpha1.BatchSandbox, pods *[]*corev1.Pod) {
			other := recoveredRuntimePod("other", "other")
			other.Status.Phase = corev1.PodFailed
			*pods = append(*pods, other)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bs := &sandboxv1alpha1.BatchSandbox{Status: sandboxv1alpha1.BatchSandboxStatus{Phase: sandboxv1alpha1.BatchSandboxPhaseFailed, FailedPodUIDs: []string{"first", "second"}}}
			setConditionInStatus(&bs.Status, sandboxv1alpha1.BatchSandboxConditionPodFailed, sandboxv1alpha1.ConditionTrue, "CrashLoopBackOff", "failed")
			pods := []*corev1.Pod{recoveredRuntimePod("sandbox-0", "first"), recoveredRuntimePod("sandbox-1", "second")}
			mutate(bs, &pods)
			view := buildRuntimeView(bs, pods)
			assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)
			assert.Equal(t, bs.Status.FailedPodUIDs, view.status.FailedPodUIDs, "keep original failure evidence")
			assert.True(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
		})
	}
}

func TestPodFailureRecoveryMultiplePods(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{Status: sandboxv1alpha1.BatchSandboxStatus{Phase: sandboxv1alpha1.BatchSandboxPhaseSucceed}}
	pods := []*corev1.Pod{recoveredRuntimePod("first", "first"), recoveredRuntimePod("second", "second")}
	failed := []*corev1.Pod{pods[0].DeepCopy(), pods[1].DeepCopy()}
	for _, pod := range failed {
		pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	}
	bs.Status = *buildRuntimeView(bs, failed).status
	require.Equal(t, []string{"first", "second"}, bs.Status.FailedPodUIDs)
	// Seeing only one recovered pod must not narrow the original provenance.
	partial := buildRuntimeView(bs, []*corev1.Pod{pods[0], failed[1]})
	require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, partial.status.Phase)
	require.Equal(t, bs.Status.FailedPodUIDs, partial.status.FailedPodUIDs)
	bs.Status = *partial.status
	recovered := buildRuntimeView(bs, pods)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, recovered.status.Phase)
	assert.Empty(t, recovered.status.FailedPodUIDs)
}

func TestResumeFailureClearsRecoveryProvenance(t *testing.T) {
	for _, phase := range []sandboxv1alpha1.BatchSandboxPhase{sandboxv1alpha1.BatchSandboxPhaseResuming, sandboxv1alpha1.BatchSandboxPhaseSucceed} {
		t.Run(string(phase), func(t *testing.T) {
			bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:   sandboxv1alpha1.BatchSandboxSpec{Pause: ptr.To(false)},
				Status: sandboxv1alpha1.BatchSandboxStatus{Phase: phase, PauseObservedGeneration: 1, FailedPodUIDs: []string{"old"}},
			}
			pod := recoveredRuntimePod("sandbox-0", "original")
			failed := pod.DeepCopy()
			failed.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			view := buildRuntimeView(bs, []*corev1.Pod{failed})
			require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)
			assert.Empty(t, view.status.FailedPodUIDs)
			assert.True(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionResumeFailed))
			bs.Status = *view.status
			assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, buildRuntimeView(bs, []*corev1.Pod{pod}).status.Phase)
		})
	}
}

func TestPodFailureRecoveryStatusPatch(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: "default"},
		Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1)), Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test"}}}}},
	}
	require.NoError(t, apiClient.Create(ctx, bs))
	var patchStatus map[string]any
	wrapped := interceptor.NewClient(apiClient, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			require.Equal(t, types.MergePatchType, patch.Type())
			data, err := patch.Data(obj)
			require.NoError(t, err)
			var payload map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &payload))
			patchStatus = nil
			require.NoError(t, json.Unmarshal(payload["status"], &patchStatus))
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &BatchSandboxReconciler{Client: wrapped, StatusRVExpectation: expectations.NewResourceVersionExpectation()}
	for _, desiredUIDs := range [][]string{nil, {"first", "second"}, nil, {"third"}, {}} {
		oldUIDs := bs.Status.FailedPodUIDs
		desired := bs.Status.DeepCopy()
		desired.FailedPodUIDs = desiredUIDs
		require.NoError(t, r.updateStatus(ctx, bs, desired))
		value, present := patchStatus["failedPodUIDs"]
		if len(oldUIDs) > 0 && len(desiredUIDs) == 0 {
			require.True(t, present, "clearing requires an explicit key")
			assert.Nil(t, value, "clearing requires JSON null")
		} else if len(desiredUIDs) == 0 {
			assert.False(t, present)
		}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(bs), bs))
		if len(desiredUIDs) == 0 {
			assert.Empty(t, bs.Status.FailedPodUIDs)
		} else {
			assert.Equal(t, desiredUIDs, bs.Status.FailedPodUIDs)
		}
	}
}
