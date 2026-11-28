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

package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestPoolDynamicClient(t *testing.T, poolName string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		SandboxPoolGVRV1Alpha2: "SandboxPoolList",
		SandboxPoolGVRV1Alpha1: "SandboxPoolList",
	})
	pool := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "SandboxPool",
			"metadata": map[string]any{
				"name":      poolName,
				"namespace": DefaultResourceNamespace,
			},
			"spec": map[string]any{
				"capacity": map[string]any{
					"poolMin":   int64(2),
					"poolMax":   int64(2),
					"bufferMin": int64(0),
					"bufferMax": int64(0),
				},
				"fastletTemplate": map[string]any{
					"spec": map[string]any{
						"affinity": map[string]any{
							"podAntiAffinity": map[string]any{
								"requiredDuringSchedulingIgnoredDuringExecution": []any{
									map[string]any{"topologyKey": "kubernetes.io/hostname"},
								},
							},
						},
						"containers": []any{
							map[string]any{
								"name":  "fastlet",
								"image": "opensandbox/fsb-fastlet:latest",
								"env": []any{
									map[string]any{"name": "EXISTING", "value": "1"},
								},
							},
						},
					},
				},
			},
		},
	}
	_, err := dyn.Resource(SandboxPoolGVRV1Alpha2).Namespace(DefaultResourceNamespace).Create(context.Background(), pool, metav1.CreateOptions{})
	require.NoError(t, err)
	return dyn
}

func makeReadyFastletPod(name, ns, poolName, nodeName string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       uid,
			Labels: map[string]string{
				"app":          "sandbox-fastlet",
				"sandbox-pool": poolName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "fastlet", Image: "opensandbox/fsb-fastlet:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "fastlet", Ready: true},
			},
		},
	}
}

func TestTriggerFastletPoolRollout_ReplacesPodsAndStampsTemplate(t *testing.T) {
	const poolName = "opensandbox-bench-pool"
	dyn := newTestPoolDynamicClient(t, poolName)
	cs := fake.NewSimpleClientset(
		makeReadyFastletPod("opensandbox-bench-pool-old1", DefaultResourceNamespace, poolName, "node-a", "uid-old-1"),
		makeReadyFastletPod("opensandbox-bench-pool-old2", DefaultResourceNamespace, poolName, "node-b", "uid-old-2"),
	)
	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs, DynamicClient: dyn})

	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = cs.CoreV1().Pods(DefaultResourceNamespace).Delete(context.Background(), "opensandbox-bench-pool-old1", metav1.DeleteOptions{})
		_ = cs.CoreV1().Pods(DefaultResourceNamespace).Delete(context.Background(), "opensandbox-bench-pool-old2", metav1.DeleteOptions{})
		_, _ = cs.CoreV1().Pods(DefaultResourceNamespace).Create(context.Background(),
			makeReadyFastletPod("opensandbox-bench-pool-new1", DefaultResourceNamespace, poolName, "node-a", "uid-new-1"), metav1.CreateOptions{})
		_, _ = cs.CoreV1().Pods(DefaultResourceNamespace).Create(context.Background(),
			makeReadyFastletPod("opensandbox-bench-pool-new2", DefaultResourceNamespace, poolName, "node-b", "uid-new-2"), metav1.CreateOptions{})
	}()

	res, err := ops.TriggerRollout(context.Background(), RolloutScenarioPool, RolloutTriggerOptions{
		PoolName:     poolName,
		Timeout:      2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.RolloutCompleted)
	require.Equal(t, RolloutScenarioPool, res.Scenario)
	require.Equal(t, []string{"opensandbox-bench-pool-old1", "opensandbox-bench-pool-old2"}, res.AffectedPods)

	// Verify both annotation and container env var were stamped on SandboxPool.
	updated, err := dyn.Resource(SandboxPoolGVRV1Alpha2).Namespace(DefaultResourceNamespace).Get(context.Background(), poolName, metav1.GetOptions{})
	require.NoError(t, err)
	annots, found, err := unstructured.NestedStringMap(updated.Object, "spec", "fastletTemplate", "metadata", "annotations")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, annots[rolloutTriggerAnnotationKey])

	containers, found, err := unstructured.NestedSlice(updated.Object, "spec", "fastletTemplate", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	envSlice, found, err := unstructured.NestedSlice(containers[0].(map[string]any), "env")
	require.NoError(t, err)
	require.True(t, found)
	var sawTriggerEnv bool
	for _, item := range envSlice {
		m := item.(map[string]any)
		if m["name"] == rolloutTriggerEnvName && m["value"] == annots[rolloutTriggerAnnotationKey] {
			sawTriggerEnv = true
		}
	}
	require.True(t, sawTriggerEnv, "expected %s env var on fastlet container", rolloutTriggerEnvName)
}

func TestTriggerMultiFastletColocation_CreatesCoLocatedPoolAndCleansUp(t *testing.T) {
	const poolName = "opensandbox-bench-pool"
	dyn := newTestPoolDynamicClient(t, poolName)
	cs := fake.NewSimpleClientset(
		makeReadyFastletPod("opensandbox-bench-pool-fastlet-a", DefaultResourceNamespace, poolName, "node-a", "uid-a"),
	)
	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs, DynamicClient: dyn})

	var createdColocName string
	var sawNodeSelector string
	var hadAffinity bool

	// Simulate SandboxPool controller spawning a co-located Fastlet Pod when the temporary pool is created,
	// and deleting the pod once the temporary pool is deleted.
	done := make(chan struct{})
	go func() {
		defer close(done)
		var podCreated bool
		for i := 0; i < 200; i++ {
			list, err := dyn.Resource(SandboxPoolGVRV1Alpha2).Namespace(DefaultResourceNamespace).List(context.Background(), metav1.ListOptions{})
			if err == nil {
				var foundColoc *unstructured.Unstructured
				for idx := range list.Items {
					if list.Items[idx].GetLabels()[colocationTempLabelKey] == "true" {
						foundColoc = &list.Items[idx]
						break
					}
				}
				if foundColoc != nil && !podCreated {
					createdColocName = foundColoc.GetName()
					_, hadAffinity, _ = unstructured.NestedMap(foundColoc.Object, "spec", "fastletTemplate", "spec", "affinity")
					sel, _, _ := unstructured.NestedStringMap(foundColoc.Object, "spec", "fastletTemplate", "spec", "nodeSelector")
					sawNodeSelector = sel["kubernetes.io/hostname"]
					_, _ = cs.CoreV1().Pods(DefaultResourceNamespace).Create(context.Background(),
						makeReadyFastletPod(createdColocName+"-pod0", DefaultResourceNamespace, createdColocName, "node-a", "uid-coloc"),
						metav1.CreateOptions{})
					podCreated = true
				} else if foundColoc == nil && podCreated {
					_ = cs.CoreV1().Pods(DefaultResourceNamespace).Delete(context.Background(), createdColocName+"-pod0", metav1.DeleteOptions{})
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	res, err := ops.TriggerRollout(context.Background(), RolloutScenarioMultiFastlet, RolloutTriggerOptions{
		PoolName:       poolName,
		Timeout:        2 * time.Second,
		ColocationHold: 30 * time.Millisecond,
		PollInterval:   10 * time.Millisecond,
	})
	<-done

	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.RolloutCompleted)
	require.Equal(t, "node-a", res.TargetNode)
	require.False(t, hadAffinity, "temporary co-located pool must strip podAntiAffinity")
	require.Equal(t, "node-a", sawNodeSelector, "temporary co-located pool must pin kubernetes.io/hostname to target node")
	require.NotEmpty(t, createdColocName)
	require.Equal(t, []string{createdColocName + "-pod0"}, res.AffectedPods)

	// Confirm the temporary SandboxPool was completely deleted on exit.
	pools, err := dyn.Resource(SandboxPoolGVRV1Alpha2).Namespace(DefaultResourceNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pools.Items, 1)
	require.Equal(t, poolName, pools.Items[0].GetName())
}

func TestTriggerRuntimeDaemonSetRollout_PatchesAndWaitsReady(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "firecracker-runtime",
			Namespace:  DefaultSystemNamespace,
			Generation: 1,
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
				},
			},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1,
			DesiredNumberScheduled: 2,
			UpdatedNumberScheduled: 0,
			NumberReady:            1,
			NumberUnavailable:      1,
		},
	}
	cs := fake.NewSimpleClientset(ds)
	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs})

	go func() {
		time.Sleep(25 * time.Millisecond)
		cur, err := cs.AppsV1().DaemonSets(DefaultSystemNamespace).Get(context.Background(), "firecracker-runtime", metav1.GetOptions{})
		if err != nil {
			return
		}
		cur.Generation = 2
		cur.Status.ObservedGeneration = 2
		cur.Status.UpdatedNumberScheduled = 2
		cur.Status.NumberReady = 2
		cur.Status.NumberUnavailable = 0
		_, _ = cs.AppsV1().DaemonSets(DefaultSystemNamespace).Update(context.Background(), cur, metav1.UpdateOptions{})
	}()

	res, err := ops.TriggerRollout(context.Background(), RolloutScenarioDaemonSet, RolloutTriggerOptions{
		DaemonSetName: "firecracker-runtime",
		Timeout:       2 * time.Second,
		PollInterval:  10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.RolloutCompleted)
	require.Equal(t, RolloutScenarioDaemonSet, res.Scenario)

	updated, err := cs.AppsV1().DaemonSets(DefaultSystemNamespace).Get(context.Background(), "firecracker-runtime", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, updated.Spec.Template.Annotations[rolloutTriggerAnnotationKey])
}

func TestTriggerFastletPoolRollout_TimeoutDiagnosesUnschedulableAndRollsBack(t *testing.T) {
	const poolName = "opensandbox-bench-pool"
	dyn := newTestPoolDynamicClient(t, poolName)
	pendingSurge := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "opensandbox-bench-pool-fastlet-surge",
			Namespace: DefaultResourceNamespace,
			UID:       "uid-surge",
			Labels:    map[string]string{"app": "sandbox-fastlet", "sandbox-pool": poolName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "fastlet", Image: "opensandbox/fsb-fastlet:latest"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Reason:  "Unschedulable",
					Message: "0/2 nodes are available: 2 node(s) didn't match pod anti-affinity rules.",
				},
			},
		},
	}
	cs := fake.NewSimpleClientset(
		makeReadyFastletPod("opensandbox-bench-pool-old1", DefaultResourceNamespace, poolName, "node-a", "uid-old-1"),
	)
	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs, DynamicClient: dyn})

	go func() {
		time.Sleep(15 * time.Millisecond)
		_, _ = cs.CoreV1().Pods(DefaultResourceNamespace).Create(context.Background(), pendingSurge, metav1.CreateOptions{})
	}()

	res, err := ops.TriggerRollout(context.Background(), RolloutScenarioPool, RolloutTriggerOptions{
		PoolName:     poolName,
		Timeout:      80 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	require.Error(t, err)
	require.NotNil(t, res)
	require.False(t, res.RolloutCompleted)
	require.Contains(t, res.Detail, "new_pending=1")
	require.Contains(t, res.Detail, "Unschedulable")
	require.Contains(t, res.Detail, "rolled back fastletTemplate")

	// Verify fastletTemplate was rolled back without BENCH_ROLLOUT_TRIGGER env.
	restored, getErr := dyn.Resource(SandboxPoolGVRV1Alpha2).Namespace(DefaultResourceNamespace).Get(context.Background(), poolName, metav1.GetOptions{})
	require.NoError(t, getErr)
	containers, found, _ := unstructured.NestedSlice(restored.Object, "spec", "fastletTemplate", "spec", "containers")
	require.True(t, found)
	envSlice, _, _ := unstructured.NestedSlice(containers[0].(map[string]any), "env")
	for _, item := range envSlice {
		require.NotEqual(t, rolloutTriggerEnvName, item.(map[string]any)["name"])
	}
}
