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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func TestDetectProvisioningFailure(t *testing.T) {
	waiting := func(reason string) *corev1.ContainerStateWaiting {
		return &corev1.ContainerStateWaiting{Reason: reason}
	}
	for _, tc := range []struct {
		name    string
		pod     *corev1.Pod
		cond    string
		reason  string
		message string
		perma   bool
		stuck   bool
	}{
		{
			name: "image pull backoff",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("ImagePullBackOff")}}}}},
			cond: recoveryConditionImagePull, reason: "ImagePullBackOff", stuck: true,
		},
		{
			name: "err image pull",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("ErrImagePull")}}}}},
			cond: recoveryConditionImagePull, reason: "ErrImagePull", stuck: true,
		},
		{
			name: "init container image pull",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "no space left on device"}}}},
				ContainerStatuses:     []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("PodInitializing")}}},
			}},
			cond: recoveryConditionImagePull, reason: "ImagePullBackOff", message: "no space left on device", stuck: true,
		},
		{
			name: "manifest not found is permanent",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: `manifest for missing:tag not found`},
			}}}}},
			cond: recoveryConditionImagePull, reason: "ErrImagePull", message: `manifest for missing:tag not found`, perma: true, stuck: true,
		},
		{
			name: "kubelet admission rejection is stuck",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:  corev1.PodFailed,
				Reason: "OutOfcpu",
			}},
			cond: recoveryConditionKubeletAdmission, reason: "OutOfcpu", stuck: true,
		},
		{
			name: "node pressure eviction is stuck",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:  corev1.PodFailed,
				Reason: "Evicted",
			}},
			cond: recoveryConditionKubeletAdmission, reason: "Evicted", stuck: true,
		},
		{
			name: "terminal container failure is not admission",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:  corev1.PodFailed,
				Reason: "ContainerFailed",
				ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
				}}},
			}},
			stuck: false,
		},
		{
			name: "create container config error excluded",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("CreateContainerConfigError")}}}}},
		},
		{
			name: "crash loop excluded",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("CrashLoopBackOff")}}}}},
		},
		{
			name: "running container not stuck",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}},
		},
		{
			name: "deleting pod not stuck",
			pod: func() *corev1.Pod {
				pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: waiting("ImagePullBackOff")}}}}}
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				return pod
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure, stuck := detectProvisioningFailure(tc.pod)
			assert.Equal(t, tc.stuck, stuck)
			assert.Equal(t, tc.cond, failure.Condition)
			assert.Equal(t, tc.reason, failure.Reason)
			assert.Equal(t, tc.message, failure.Message)
			assert.Equal(t, tc.perma, failure.Permanent)
		})
	}
}

func TestIsPermanentImagePullFailure(t *testing.T) {
	for _, tc := range []struct {
		message   string
		permanent bool
	}{
		{message: `Back-off pulling image "nginx:missing": manifest for nginx:missing not found`, permanent: true},
		{message: `pull access denied for private, repository does not exist or may require authorization`, permanent: true},
		{message: `failed to resolve reference "reg.example.com/foo:latest": expected at most 1, got 2`, permanent: false},
		{message: `image verification failed: authentication required`, permanent: true},
		{message: `failed to prepare extraction snapshot: no space left on device`, permanent: false},
		{message: `Get "https://reg.example.com/v2/": dial tcp 1.2.3.4:443: i/o timeout`, permanent: false},
		{message: "", permanent: false},
	} {
		assert.Equal(t, tc.permanent, isPermanentImagePullFailure(tc.message), tc.message)
	}
}

func TestPodRecoveryBudget(t *testing.T) {
	store := &podRecoveryBudgetStore{budgets: map[string]*podRecoveryBudget{}}
	key := "ns/budget-sandbox"
	assert.Equal(t, 0, store.count(key, 1))
	store.increment(key, 1)
	store.increment(key, 1)
	assert.Equal(t, 2, store.count(key, 1))
	// A generation mismatch (template update) resets the budget.
	assert.Equal(t, 0, store.count(key, 2))
	// Deletion removes the entry entirely.
	store.delete(key)
	assert.Equal(t, 0, store.count(key, 2))
}

func TestPodRecoveryBudgetBounded(t *testing.T) {
	store := &podRecoveryBudgetStore{budgets: map[string]*podRecoveryBudget{}}
	for i := 0; i <= podRecoveryBudgetMaxEntries; i++ {
		store.increment(fmt.Sprintf("ns/sandbox-%d", i), 1)
	}
	assert.LessOrEqual(t, len(store.budgets), podRecoveryBudgetMaxEntries)
}

func TestReconcilePodRecovery(t *testing.T) {
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

	newReconciler := func() *BatchSandboxReconciler {
		featureCfg := NewFeatureConfig()
		featureCfg.Load(map[string]string{
			featureConfigKeyPodRecoveryStuckThreshold: time.Minute.String(),
			featureConfigKeyPodRecoveryMaxAttempts:    "3",
		})
		return &BatchSandboxReconciler{Client: manager.GetClient(), Scheme: testscheme,
			Recorder: record.NewFakeRecorder(100), StatusRVExpectation: expectations.NewResourceVersionExpectation(),
			FeatureConfig: featureCfg}
	}

	type harness struct {
		bs        *sandboxv1alpha1.BatchSandbox
		pod       *corev1.Pod
		r         *BatchSandboxReconciler
		sync      func(object client.Object)
		events    func() []string
		reconcile func()
	}

	setup := func(t *testing.T, name string) *harness {
		t.Helper()
		bs := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1)),
				Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:latest"}}}},
			},
		}
		require.NoError(t, apiClient.Create(testContext, bs))
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: "default",
			Labels:          map[string]string{labelBatchSandboxNameKey: name, labelBatchSandboxPodIndexKey: "0"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bs, sandboxv1alpha1.GroupVersion.WithKind("BatchSandbox"))},
		}, Spec: *bs.Spec.Template.Spec.DeepCopy()}
		require.NoError(t, apiClient.Create(testContext, pod))
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Image: "busybox:latest",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "no space left on device"}},
		}}
		require.NoError(t, apiClient.Status().Update(testContext, pod))
		r := newReconciler()
		h := &harness{bs: bs, pod: pod, r: r}
		key := client.ObjectKeyFromObject(bs)
		syncObject := func(object client.Object) {
			t.Helper()
			require.Eventually(t, func() bool {
				cached := object.DeepCopyObject().(client.Object)
				return r.Get(testContext, client.ObjectKeyFromObject(object), cached) == nil && cached.GetResourceVersion() == object.GetResourceVersion()
			}, 5*time.Second, 20*time.Millisecond)
		}
		h.sync = syncObject
		h.events = func() []string {
			var messages []string
			for {
				select {
				case entry := <-r.Recorder.(*record.FakeRecorder).Events:
					messages = append(messages, entry)
				default:
					return messages
				}
			}
		}
		h.reconcile = func() {
			t.Helper()
			syncObject(bs)
			_, err := r.Reconcile(testContext, ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			require.NoError(t, apiClient.Get(testContext, key, bs))
		}
		syncObject(pod)
		return h
	}

	t.Run("replaces stuck pod and recreates from template", func(t *testing.T) {
		h := setup(t, "recovery-replace")
		replacementsBefore := testutil.ToFloat64(podRecoveryReplacements.WithLabelValues(recoveryConditionImagePull))

		h.reconcile()
		require.NoError(t, apiClient.Get(testContext, client.ObjectKeyFromObject(h.pod), h.pod))
		require.Nil(t, h.pod.DeletionTimestamp)
		require.Equal(t, sandboxv1alpha1.BatchSandboxPhasePending, h.bs.Status.Phase)

		oldUID := h.pod.UID
		h.r.podRecoveryNow = func() time.Time { return time.Now().Add(10 * time.Minute) }
		h.reconcile()
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(h.r.Get(testContext, client.ObjectKeyFromObject(h.pod), &corev1.Pod{}))
		}, 5*time.Second, 20*time.Millisecond)
		require.Equal(t, 1, podRecoveryBudgets.count(controllerutils.GetControllerKey(h.bs), h.bs.Generation))
		require.InDelta(t, replacementsBefore+1, testutil.ToFloat64(podRecoveryReplacements.WithLabelValues(recoveryConditionImagePull)), 0.001)
		assert.True(t, strings.Contains(strings.Join(h.events(), "\n"), "Normal ReplacedStuckPod"))

		h.reconcile()
		replacement := &corev1.Pod{}
		require.Eventually(t, func() bool {
			return h.r.Get(testContext, client.ObjectKeyFromObject(h.pod), replacement) == nil
		}, 5*time.Second, 20*time.Millisecond)
		require.NotEqual(t, oldUID, replacement.UID)

		replacement.Status.Phase = corev1.PodRunning
		replacement.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		require.NoError(t, apiClient.Status().Update(testContext, replacement))
		h.sync(replacement)
		h.reconcile()
		require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, h.bs.Status.Phase)
		// The next reconcile after leaving Pending drops the budget entry.
		h.reconcile()
		_, ok := podRecoveryBudgets.budgets[controllerutils.GetControllerKey(h.bs)]
		require.False(t, ok)
	})

	t.Run("respects replacement budget and resets on template update", func(t *testing.T) {
		h := setup(t, "recovery-limit")
		key := controllerutils.GetControllerKey(h.bs)
		for i := 0; i < 3; i++ {
			podRecoveryBudgets.increment(key, h.bs.Generation)
		}

		h.r.podRecoveryNow = func() time.Time { return time.Now().Add(10 * time.Minute) }
		h.reconcile()
		require.NoError(t, apiClient.Get(testContext, client.ObjectKeyFromObject(h.pod), h.pod))
		require.Nil(t, h.pod.DeletionTimestamp)
		assert.True(t, strings.Contains(strings.Join(h.events(), "\n"), "Warning PodRecoveryLimitReached"))

		h.bs.Spec.Template.Spec.Containers[0].Image = "busybox:1.37"
		require.NoError(t, apiClient.Update(testContext, h.bs))
		h.reconcile()
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(h.r.Get(testContext, client.ObjectKeyFromObject(h.pod), &corev1.Pod{}))
		}, 5*time.Second, 20*time.Millisecond)
		require.Equal(t, 1, podRecoveryBudgets.count(key, h.bs.Generation))
	})

	t.Run("skips replacement for permanently broken images", func(t *testing.T) {
		h := setup(t, "recovery-permanent")
		h.pod.Status.ContainerStatuses[0].State.Waiting.Message = `Back-off pulling image "missing:tag": manifest for missing:tag not found`
		require.NoError(t, apiClient.Status().Update(testContext, h.pod))
		h.sync(h.pod)
		h.r.podRecoveryNow = func() time.Time { return time.Now().Add(10 * time.Minute) }
		h.reconcile()
		require.NoError(t, apiClient.Get(testContext, client.ObjectKeyFromObject(h.pod), h.pod))
		require.Nil(t, h.pod.DeletionTimestamp)
		require.Equal(t, 0, podRecoveryBudgets.count(controllerutils.GetControllerKey(h.bs), h.bs.Generation))
		assert.True(t, strings.Contains(strings.Join(h.events(), "\n"), "Warning ImagePullPermanentFailure"))
	})

	t.Run("recovers kubelet admission rejections without freezing", func(t *testing.T) {
		h := setup(t, "recovery-admission")
		h.pod.Status.Phase = corev1.PodFailed
		h.pod.Status.Reason = "OutOfcpu"
		h.pod.Status.Message = "Insufficient cpu (limit 1000m)"
		require.NoError(t, apiClient.Status().Update(testContext, h.pod))
		h.sync(h.pod)

		// The rejection must not flip the sandbox to Failed; replacement proceeds.
		h.r.podRecoveryNow = func() time.Time { return time.Now().Add(10 * time.Minute) }
		h.reconcile()
		require.Equal(t, sandboxv1alpha1.BatchSandboxPhasePending, h.bs.Status.Phase)
		require.False(t, hasTrueBatchSandboxCondition(h.bs.Status.Conditions, sandboxv1alpha1.BatchSandboxConditionPodFailed))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(h.r.Get(testContext, client.ObjectKeyFromObject(h.pod), &corev1.Pod{}))
		}, 5*time.Second, 20*time.Millisecond)
		assert.True(t, strings.Contains(strings.Join(h.events(), "\n"), "Normal ReplacedStuckPod"))

		h.reconcile()
		replacement := &corev1.Pod{}
		require.Eventually(t, func() bool {
			return h.r.Get(testContext, client.ObjectKeyFromObject(h.pod), replacement) == nil
		}, 5*time.Second, 20*time.Millisecond)
		replacement.Status.Phase = corev1.PodRunning
		replacement.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		require.NoError(t, apiClient.Status().Update(testContext, replacement))
		h.sync(replacement)
		h.reconcile()
		require.Equal(t, sandboxv1alpha1.BatchSandboxPhaseSucceed, h.bs.Status.Phase)
	})
}
