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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

// The release patch and the status patch of one reconcile are both guarded by
// resourceVersion, so these cases run against a real API server.
func TestReleasePodsAgainstAPIServer(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })

	apiClient, err := client.New(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()

	// newTaskSandbox creates a running task sandbox and returns the reconciler
	// with its recorder and the object as a reconcile would have read it.
	newTaskSandbox := func(t *testing.T, name string, annotations map[string]string) (*BatchSandboxReconciler, *record.FakeRecorder, *sandboxv1alpha1.BatchSandbox) {
		t.Helper()
		bs := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations},
			Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
		}
		require.NoError(t, apiClient.Create(ctx, bs))
		bs.Status = sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhaseSucceed, Replicas: 1, Allocated: 1, Ready: 1,
		}
		require.NoError(t, apiClient.Status().Update(ctx, bs))
		recorder := record.NewFakeRecorder(10)
		r := &BatchSandboxReconciler{
			Client:              apiClient,
			Recorder:            recorder,
			StatusRVExpectation: expectations.NewResourceVersionExpectation(),
		}
		return r, recorder, bs.DeepCopy()
	}

	// Reconcile releases finished task pods and then patches status from the
	// same in-memory object. The release must leave that object current, or
	// the status write of the same pass always conflicts and the task counters
	// it carries are dropped until a later reconcile.
	t.Run("status patch after a release persists", func(t *testing.T) {
		r, _, cached := newTaskSandbox(t, "released", nil)

		require.NoError(t, r.releasePods(ctx, cached, []string{"pod-0"}))
		released, err := parseSandboxReleased(cached)
		require.NoError(t, err)
		assert.Equal(t, []string{"pod-0"}, released.Pods)

		pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		view := buildRuntimeView(cached, []*corev1.Pod{pod})
		view.status.TaskSucceed = 1
		_, errs := r.persistRuntimeView(ctx, cached, view)
		require.Empty(t, errs)

		latest := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(cached), latest))
		assert.Equal(t, int32(1), latest.Status.TaskSucceed)
		released, err = parseSandboxReleased(latest)
		require.NoError(t, err)
		assert.Equal(t, []string{"pod-0"}, released.Pods)
	})

	// The release must not launder a concurrent change into the status patch's
	// optimistic lock: when the object changed after it was read, the release
	// conflicts and leaves the in-memory object as it was.
	t.Run("release conflicts with a change made after the read", func(t *testing.T) {
		r, recorder, cached := newTaskSandbox(t, "changed", nil)
		readVersion := cached.ResourceVersion

		other := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: cached.Namespace, Name: cached.Name}}
		require.NoError(t, apiClient.Patch(ctx, other, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"renewed":"true"}}}`))))

		err := r.releasePods(ctx, cached, []string{"pod-0"})
		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err), "%v", err)
		assert.Equal(t, readVersion, cached.ResourceVersion)
		assert.NotContains(t, cached.Annotations, annoAllocReleaseKey)
		assert.Len(t, recorder.Events, 1, "only the failure event")
	})

	// Every reconcile reports every released task again. Writing the
	// annotation is only needed when that set grows.
	t.Run("unchanged release set is not written", func(t *testing.T) {
		r, recorder, cached := newTaskSandbox(t, "unchanged", map[string]string{annoAllocReleaseKey: `{"pods":["pod-0"]}`})
		readVersion := cached.ResourceVersion

		require.NoError(t, r.releasePods(ctx, cached, []string{"pod-0"}))

		latest := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(cached), latest))
		assert.Equal(t, readVersion, latest.ResourceVersion, "no write for an unchanged release set")
		assert.Empty(t, recorder.Events)
	})
}
