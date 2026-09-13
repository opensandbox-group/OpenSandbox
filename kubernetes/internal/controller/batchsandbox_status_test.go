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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func TestDeletingPodFailureAttribution(t *testing.T) {
	for _, phase := range []sandboxv1alpha1.BatchSandboxPhase{
		sandboxv1alpha1.BatchSandboxPhasePending,
		sandboxv1alpha1.BatchSandboxPhaseResuming,
		sandboxv1alpha1.BatchSandboxPhaseSucceed,
	} {
		for _, deleting := range []bool{false, true} {
			t.Run(string(phase)+map[bool]string{false: "/active", true: "/deleting"}[deleting], func(t *testing.T) {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "sandbox-0"},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
						ContainerStatuses: []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"},
						}}},
					},
				}
				if deleting {
					now := metav1.Now()
					pod.DeletionTimestamp = &now
				}
				bs := &sandboxv1alpha1.BatchSandbox{Status: sandboxv1alpha1.BatchSandboxStatus{Phase: phase}}
				view := buildRuntimeView(bs, []*corev1.Pod{pod})
				if deleting {
					assert.NotEqual(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)
					assert.False(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
					assert.False(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionResumeFailed))
				} else {
					assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, view.status.Phase)
					assert.True(t, hasTrueBatchSandboxCondition(view.status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
				}
			})
		}
	}
}

func TestDeletingPodWaitingFailure(t *testing.T) {
	for _, reason := range []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError"} {
		t.Run(reason, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
			}}}}
			_, _, failed := getPodFailureReasonAndMessage(pod)
			assert.True(t, failed)
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			_, _, failed = getPodFailureReasonAndMessage(pod)
			assert.False(t, failed)
		})
	}
}

// Run the real reconciler against an API server, but drive reconciles explicitly
// so both orders of failure/deletion observation are deterministic. No kubelet
// runs in envtest; the test publishes its terminal Pod status and releases its finalizer.
func TestReconcileDeletingPod(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })
	testContext, stop := context.WithCancel(context.Background())
	manager, err := ctrl.NewManager(config, ctrl.Options{Scheme: testscheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	require.NoError(t, err)
	require.NoError(t, fieldindex.RegisterFieldIndexes(manager.GetCache()))
	managerDone := make(chan error, 1)
	go func() { managerDone <- manager.Start(testContext) }()
	t.Cleanup(func() { stop(); require.NoError(t, <-managerDone) })
	require.True(t, manager.GetCache().WaitForCacheSync(testContext))
	apiClient, err := client.New(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)

	for _, failureBeforeDeletion := range []bool{false, true} {
		name := "resume-deleting"
		if failureBeforeDeletion {
			name = "resume-real-failure"
		}
		t.Run(name, func(t *testing.T) {
			bs := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1)), Pause: ptr.To(false),
					Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "snapshot:test"}}}},
				},
			}
			require.NoError(t, apiClient.Create(testContext, bs))
			bs.Status.Phase = sandboxv1alpha1.BatchSandboxPhaseResuming
			bs.Status.PauseObservedGeneration = bs.Generation
			require.NoError(t, apiClient.Status().Update(testContext, bs))
			snapshot := &sandboxv1alpha1.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name + "-pause", Namespace: "default"},
				Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: name},
			}
			require.NoError(t, apiClient.Create(testContext, snapshot))
			snapshot.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseSucceed
			snapshot.Status.Containers = []sandboxv1alpha1.ContainerSnapshot{{ContainerName: "main", ImageURI: "snapshot:test"}}
			require.NoError(t, apiClient.Status().Update(testContext, snapshot))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: "default",
				Finalizers:      []string{"test.opensandbox.io/hold"},
				Labels:          map[string]string{LabelBatchSandboxNameKey: name, LabelBatchSandboxPodIndexKey: "0"},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bs, sandboxv1alpha1.GroupVersion.WithKind("BatchSandbox"))},
			}, Spec: *bs.Spec.Template.Spec.DeepCopy()}
			require.NoError(t, apiClient.Create(testContext, pod))
			oldUID := pod.UID
			r := &BatchSandboxReconciler{Client: manager.GetClient(), Scheme: testscheme,
				Recorder: record.NewFakeRecorder(100), StatusRVExpectation: expectations.NewResourceVersionExpectation()}
			key := client.ObjectKeyFromObject(bs)
			syncObject := func(object client.Object) {
				t.Helper()
				require.Eventually(t, func() bool {
					cached := object.DeepCopyObject().(client.Object)
					return r.Get(testContext, client.ObjectKeyFromObject(object), cached) == nil && cached.GetResourceVersion() == object.GetResourceVersion()
				}, 5*time.Second, 20*time.Millisecond)
			}
			markFailed := func() {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Image: "snapshot:test", ImageID: "snapshot:test",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
				}}
				require.NoError(t, apiClient.Status().Update(testContext, pod))
				syncObject(pod)
			}
			reconcile := func() {
				t.Helper()
				syncObject(bs)
				_, err := r.Reconcile(testContext, ctrl.Request{NamespacedName: key})
				require.NoError(t, err)
				require.NoError(t, apiClient.Get(testContext, key, bs))
			}
			syncObject(snapshot)
			if failureBeforeDeletion {
				markFailed()
				reconcile()
				require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, bs.Status.Phase)
			}
			require.NoError(t, apiClient.Delete(testContext, pod, client.GracePeriodSeconds(0)))
			require.NoError(t, apiClient.Get(testContext, client.ObjectKeyFromObject(pod), pod))
			require.NotNil(t, pod.DeletionTimestamp)
			if !failureBeforeDeletion {
				markFailed()
			} else {
				syncObject(pod)
			}
			reconcile()
			if failureBeforeDeletion {
				require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, bs.Status.Phase)
				require.True(t, hasTrueBatchSandboxCondition(bs.Status.Conditions, sandboxv1alpha1.BatchSandboxConditionResumeFailed))
				return
			}
			require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, bs.Status.Phase)
			require.False(t, hasTrueBatchSandboxCondition(bs.Status.Conditions, sandboxv1alpha1.BatchSandboxConditionResumeFailed))
			require.False(t, hasTrueBatchSandboxCondition(bs.Status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
			pod.Finalizers = nil
			require.NoError(t, apiClient.Update(testContext, pod))
			require.Eventually(t, func() bool {
				return apierrors.IsNotFound(r.Get(testContext, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
			}, 5*time.Second, 20*time.Millisecond)
			reconcile()
			replacement := &corev1.Pod{}
			require.NoError(t, apiClient.Get(testContext, client.ObjectKeyFromObject(pod), replacement))
			require.NotEmpty(t, replacement.UID)
			require.NotEqual(t, oldUID, replacement.UID)
			replacement.Status.Phase = corev1.PodRunning
			replacement.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			require.NoError(t, apiClient.Status().Update(testContext, replacement))
			syncObject(replacement)
			reconcile()
			require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, bs.Status.Phase)
		})
	}
}
