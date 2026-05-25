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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func newTestSnapshotReconciler(objs ...client.Object) *SandboxSnapshotReconciler {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.SandboxSnapshot{}).
		WithObjects(objs...).
		Build()

	return &SandboxSnapshotReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

func TestSandboxSnapshotHandleCommitting_SetsSucceedReadyCondition(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test:tag"},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot-commit",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r := newTestSnapshotReconciler(snapshot, job)

	result, err := r.handleCommitting(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseSucceed, updated.Status.Phase)

	foundReady := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.SandboxSnapshotConditionReady {
			foundReady = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "SnapshotReady", cond.Reason)
			assert.NotNil(t, cond.LastTransitionTime)
		}
		if cond.Type == sandboxv1alpha1.SandboxSnapshotConditionFailed {
			assert.NotEqual(t, sandboxv1alpha1.ConditionTrue, cond.Status)
		}
	}
	assert.True(t, foundReady, "Ready condition should be set after successful commit")
}

func TestSandboxSnapshotHandleCommitting_PersistsImageDigestsFromTerminationMessage(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-main:tag"},
				{ContainerName: "sidecar", ImageURI: "registry/test-sidecar:tag"},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot-commit",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}
	commitPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot-commit-abcde",
			Namespace: "default",
			Labels: map[string]string{
				"job-name": "test-snapshot-commit",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: CommitJobContainerName,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
							Message:  `{"containers":[{"name":"main","image":"registry/test-main:tag","digest":"sha256:main"},{"name":"sidecar","image":"registry/test-sidecar:tag","digest":"sha256:sidecar"}]}`,
						},
					},
				},
			},
		},
	}

	r := newTestSnapshotReconciler(snapshot, job, commitPod)

	result, err := r.handleCommitting(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseSucceed, updated.Status.Phase)
	require.Len(t, updated.Status.Containers, 2)
	assert.Equal(t, "sha256:main", updated.Status.Containers[0].ImageDigest)
	assert.Equal(t, "sha256:sidecar", updated.Status.Containers[1].ImageDigest)
}

func TestUpdateSnapshotStatus_DoesNotDowngradeSucceededSnapshot(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed,
			Conditions: []sandboxv1alpha1.SandboxSnapshotCondition{
				{
					Type:   sandboxv1alpha1.SandboxSnapshotConditionReady,
					Status: sandboxv1alpha1.ConditionTrue,
					Reason: "SnapshotReady",
				},
			},
		},
	}
	r := newTestSnapshotReconciler(snapshot)

	err := r.updateSnapshotStatus(context.Background(), snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "CommitJobFailed", "late failure")
	require.NoError(t, err)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseSucceed, updated.Status.Phase)
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.SandboxSnapshotConditionFailed {
			assert.NotEqual(t, sandboxv1alpha1.ConditionTrue, cond.Status)
		}
	}
}

func TestSandboxSnapshotHandleCommitting_KeepsRetryingWhenJobHasNotTerminallyFailed(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test:tag"},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot-commit",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Active: 1,
			Failed: 1,
		},
	}

	r := newTestSnapshotReconciler(snapshot, job)

	result, err := r.handleCommitting(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, updated.Status.Phase)
}

func TestSandboxSnapshotHandleCommitting_CreatesUnpauseJobWhenCommitJobFailed(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase:          sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
			SourcePodName:  "source-pod",
			SourceNodeName: "node-a",
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-main:tag"},
				{ContainerName: "sidecar", ImageURI: "registry/test-sidecar:tag"},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot-commit",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "DeadlineExceeded",
					Message: "Job was active longer than specified deadline",
				},
			},
		},
	}

	r := newTestSnapshotReconciler(snapshot, job)

	result, err := r.handleCommitting(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	cleanupJob := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot-unpause", Namespace: "default"}, cleanupJob))
	require.Len(t, cleanupJob.Spec.Template.Spec.Containers, 1)
	cleanupContainer := cleanupJob.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{"/usr/local/bin/image-committer"}, cleanupContainer.Command)
	assert.Equal(t, []string{"unpause", "source-pod", "default", "main", "sidecar"}, cleanupContainer.Args)
	assert.Equal(t, "node-a", cleanupJob.Spec.Template.Spec.NodeName)
}

func TestSandboxSnapshotHandlePending_MissingRegistrySetsFailedCondition(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhasePending,
		},
	}

	r := newTestSnapshotReconciler(snapshot)

	result, err := r.handlePending(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseFailed, updated.Status.Phase)

	foundFailed := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.SandboxSnapshotConditionFailed {
			foundFailed = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "RegistryNotConfigured", cond.Reason)
			assert.Contains(t, cond.Message, "snapshot-registry")
		}
	}
	assert.True(t, foundFailed, "Failed condition should be set when registry config is missing")
}

func TestSandboxSnapshotHandlePending_UsesSourcePodContainersWhenTemplateMissing(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        types.UID("test-bs-uid"),
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			PoolRef: "test-pool",
		},
	}
	setSandboxAllocation(bs, SandboxAllocation{Pods: []string{"pool-pod"}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{
				{Name: "sandbox-container", Image: "pool-image:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "sandbox.opensandbox.io/v1alpha1",
					Kind:               "BatchSandbox",
					Name:               "test-bs",
					UID:                types.UID("test-bs-uid"),
					Controller:         ptrToBool(true),
					BlockOwnerDeletion: ptrToBool(true),
				},
			},
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{
			SandboxName: "test-bs",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhasePending,
		},
	}

	r := newTestSnapshotReconciler(bs, pod, snapshot)
	r.SnapshotRegistry = "registry.default.svc.cluster.local:5000"

	result, err := r.handlePending(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, time.Second, result.RequeueAfter)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot", Namespace: "default"}, updated))
	assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, updated.Status.Phase)
	assert.Equal(t, "pool-pod", updated.Status.SourcePodName)
	assert.Equal(t, "node-a", updated.Status.SourceNodeName)
	require.Len(t, updated.Status.Containers, 1)
	assert.Equal(t, "sandbox-container", updated.Status.Containers[0].ContainerName)
	assert.Equal(t, "registry.default.svc.cluster.local:5000/test-bs-sandbox-container:snap-gen2", updated.Status.Containers[0].ImageURI)

	job := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-snapshot-commit", Namespace: "default"}, job))
}

func TestSandboxSnapshotHandlePending_PublicSnapshotUsesSnapshotIDTag(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sandbox", Image: "python:3.11"},
					},
				},
			},
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
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{
				{Name: "sandbox", Image: "python:3.11"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "osb-snap-11111111222243338444555555555555",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{
			SandboxName: "test-bs",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhasePending,
		},
	}

	r := newTestSnapshotReconciler(bs, pod, snapshot)
	r.SnapshotRegistry = "registry.default.svc.cluster.local:5000"

	result, err := r.handlePending(context.Background(), snapshot)
	require.NoError(t, err)
	assert.Equal(t, time.Second, result.RequeueAfter)

	updated := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: snapshot.Name, Namespace: "default"}, updated))
	require.Len(t, updated.Status.Containers, 1)
	assert.Equal(
		t,
		"registry.default.svc.cluster.local:5000/test-bs-sandbox:snap-11111111222243338444555555555555",
		updated.Status.Containers[0].ImageURI,
	)
}

func TestBuildCommitJob_SetsBoundedBackoffLimit(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			SourcePodName:  "test-pod",
			SourceNodeName: "node-1",
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{
					ContainerName: "main",
					ImageURI:      "registry.example.com/test:tag",
				},
			},
		},
	}

	r := newTestSnapshotReconciler(snapshot)
	r.SnapshotPushSecret = "registry-snapshot-push-secret"

	job, err := r.buildCommitJob(snapshot)
	require.NoError(t, err)
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, DefaultCommitJobBackoffLimit, *job.Spec.BackoffLimit)
	assert.Equal(t, fmt.Sprintf("%s-commit", snapshot.Name), job.Name)
}

func TestBuildCommitJob_ExecutesImageCommitterDirectlyWithIsolatedArgs(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			SourcePodName:  "pod-with-shell-chars-$(touch /tmp/nope)",
			SourceNodeName: "node-1",
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{
					ContainerName: "main;echo nope",
					ImageURI:      "registry.example.com/test:tag",
				},
			},
		},
	}

	r := newTestSnapshotReconciler(snapshot)
	r.SnapshotRegistryInsecure = true

	job, err := r.buildCommitJob(snapshot)
	require.NoError(t, err)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)

	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{"/usr/local/bin/image-committer"}, container.Command)
	assert.Equal(t, []string{
		"pod-with-shell-chars-$(touch /tmp/nope)",
		"default",
		"main;echo nope:registry.example.com/test:tag",
	}, container.Args)
	assert.Contains(t, container.Env, corev1.EnvVar{Name: "SNAPSHOT_REGISTRY_INSECURE", Value: "true"})
	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
}
