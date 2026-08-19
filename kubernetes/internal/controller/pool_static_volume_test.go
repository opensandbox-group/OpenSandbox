// Copyright 2026 Alibaba Group Holding Ltd.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
)

func TestCreatePoolPodPreservesStaticPVC(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))

	var createdPod *corev1.Pod
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				pod, ok := obj.(*corev1.Pod)
				require.True(t, ok)
				pod.Name = "shared-workspace-pool-test"
				createdPod = pod.DeepCopy()
				return nil
			},
		}).
		Build()

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-workspace-pool",
			Namespace: "opensandbox",
			UID:       types.UID("pool-uid"),
		},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sandbox-container",
							Image: "python:3.11",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "shared-workspace", MountPath: "/workspace"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "shared-workspace",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "shared-workspace-pvc",
								},
							},
						},
					},
				},
			},
		},
	}
	defer PoolScaleExpectations.DeleteExpectations(controllerutils.GetControllerKey(pool))

	r := &PoolReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
	require.NoError(t, r.createPoolPod(context.Background(), pool, "revision-1"))
	require.NotNil(t, createdPod)
	require.Len(t, createdPod.Spec.Volumes, 1)
	require.NotNil(t, createdPod.Spec.Volumes[0].PersistentVolumeClaim)
	assert.Equal(t, "shared-workspace-pvc", createdPod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	require.Len(t, createdPod.Spec.Containers, 1)
	assert.Contains(t, createdPod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      "shared-workspace",
		MountPath: "/workspace",
	})
}
