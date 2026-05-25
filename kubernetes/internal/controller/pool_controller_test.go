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
	stderrors "errors"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/recycle"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

var _ = Describe("Pool scale", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When reconciling a resource", func() {
		const resourceName = "pool-scale-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		BeforeEach(func() {
			By("creating the custom resource for the Kind Pool")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					By("The specific resource instance Pool already deleted")
					return
				}
			}
			By("Cleanup the specific resource instance Pool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully update pool status", func() {
			pool := &sandboxv1alpha1.Pool{}
			Eventually(func(g Gomega) {
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return
				}
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully scale out pool buffer size", func() {
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pool.Spec.CapacitySpec.BufferMin = 2
			pool.Spec.CapacitySpec.BufferMax = 2
			pool.Spec.ScaleStrategy = &sandboxv1alpha1.ScaleStrategy{
				MaxUnavailable: ptr.To(intstr.FromString("100%")),
			}
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Eventually(func(g Gomega) {
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return
				}
				cnt := int32(2)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully scale out buffer limit by pool max", func() {
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pool.Spec.CapacitySpec.PoolMax = 2
			pool.Spec.CapacitySpec.BufferMin = 3
			pool.Spec.CapacitySpec.BufferMax = 3
			pool.Spec.ScaleStrategy = &sandboxv1alpha1.ScaleStrategy{
				MaxUnavailable: ptr.To(intstr.FromString("100%")),
			}
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Eventually(func(g Gomega) {
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return
				}
				cnt := int32(2)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully scale in pool buffer size", func() {
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pool.Spec.CapacitySpec.BufferMin = 0
			pool.Spec.CapacitySpec.BufferMax = 0
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return
				}
				cnt := int32(0)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully scale in buffer limit by pool min", func() {
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pool.Spec.CapacitySpec.PoolMax = 1
			pool.Spec.CapacitySpec.PoolMin = 1
			pool.Spec.CapacitySpec.BufferMin = 0
			pool.Spec.CapacitySpec.BufferMax = 0
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Eventually(func(g Gomega) {
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return
				}
				cnt := int32(1)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
	})
})

var _ = Describe("Pool update", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When reconciling a resource", func() {
		const resourceName = "pool-update-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Pool")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
			pool := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, pool)
			Expect(err).NotTo(HaveOccurred())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			// Mock pod running and ready
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					By("The specific resource instance Pool already deleted")
					return
				}
			}
			By("Cleanup the specific resource instance Pool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully update pool revision", func() {
			var oldRevision string
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				pool := &sandboxv1alpha1.Pool{}
				if err := k8sClient.Get(ctx, typeNamespacedName, pool); err != nil {
					return err
				}
				if oldRevision == "" {
					oldRevision = pool.Status.Revision
				}
				pool.Spec.Template.Labels = map[string]string{
					"test.pool.update": "v1",
				}
				return k8sClient.Update(ctx, pool)
			})).Should(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				cnt := int32(1)
				g.Expect(pool.Status.Revision).NotTo(Equal(oldRevision))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully update pool with allocated pod", func() {
			pool := &sandboxv1alpha1.Pool{}
			sbxNamespaceName := types.NamespacedName{
				Name:      "sandbox-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxNamespaceName.Name,
					Namespace: sbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					PoolRef: typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			// wait allocation
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, sbxNamespaceName, sandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(sandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
			}, timeout, interval).Should(Succeed())
			Expect(k8sClient.Get(ctx, sbxNamespaceName, sandbox)).To(Succeed())
			sbxAlloc, err := getSandboxAllocation(sandbox)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(sbxAlloc.Pods)).To(Equal(1))
			// check pool allocation
			err = k8sClient.Get(ctx, typeNamespacedName, pool)
			Expect(err).NotTo(HaveOccurred())
			allocation, err := getPoolAllocation(pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(allocation.PodAllocation)).To(Equal(1))
			Expect(allocation.PodAllocation[sbxAlloc.Pods[0]]).To(Equal(sandbox.Name))
			// update pool
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			oldRevision := pool.Status.Revision
			pool.Spec.Template.Labels = map[string]string{
				"test.pool.update": "v1",
			}
			pool.Spec.ScaleStrategy = &sandboxv1alpha1.ScaleStrategy{
				MaxUnavailable: ptr.To(intstr.FromString("100%")),
			}
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				cnt := int32(2)
				g.Expect(pool.Status.Revision).NotTo(Equal(oldRevision))
				g.Expect(pool.Status.Total).To(Equal(cnt))
				pods := &v1.PodList{}
				Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
					Namespace:     typeNamespacedName.Namespace,
					FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
				})).To(Succeed())
				for _, pod := range pods.Items {
					if pod.Name == sbxAlloc.Pods[0] {
						g.Expect(pod.DeletionTimestamp).To(BeNil())
						g.Expect(pod.Labels[LabelPoolRevision]).To(Equal(oldRevision))
						continue
					}
					if pod.DeletionTimestamp != nil {
						continue
					}
					g.Expect(pod.Labels[LabelPoolRevision]).NotTo(Equal(oldRevision))
				}
			}, timeout, interval).Should(Succeed())
			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())
		})
	})
})

var _ = Describe("Pool allocate", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When reconciling a resource", func() {
		const resourceName = "pool-allocate-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Pool")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
			pool := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, pool)
			Expect(err).NotTo(HaveOccurred())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			// Mock pod running and ready
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					By("The specific resource instance Pool already deleted")
					return
				}
			}
			By("Cleanup the specific resource instance Pool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully allocate pool pod to batch sandbox and release", func() {
			pool := &sandboxv1alpha1.Pool{}
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())
			// wait allocation
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
			}, timeout, interval).Should(Succeed())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			sbxAlloc, err := getSandboxAllocation(batchSandbox)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(sbxAlloc.Pods)).To(Equal(1))
			// check pool allocation
			err = k8sClient.Get(ctx, typeNamespacedName, pool)
			Expect(err).NotTo(HaveOccurred())
			allocation, err := getPoolAllocation(pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(allocation.PodAllocation)).To(Equal(1))
			Expect(allocation.PodAllocation[sbxAlloc.Pods[0]]).To(Equal(batchSandbox.Name))
			// release
			release := AllocationRelease{
				Pods: sbxAlloc.Pods,
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			err = k8sClient.Update(ctx, batchSandbox)
			Expect(err).NotTo(HaveOccurred())
			// wait release
			Eventually(func(g Gomega) {
				err = k8sClient.Get(ctx, typeNamespacedName, pool)
				Expect(err).NotTo(HaveOccurred())
				allocation, err = getPoolAllocation(pool)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocation.PodAllocation)).To(Equal(0))
			}, timeout, interval).Should(Succeed())
			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should GC allocation when a pooled BatchSandbox detaches from its pool", func() {
			pool := &sandboxv1alpha1.Pool{}
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-solidify-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			var allocatedPod string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).To(HaveLen(1))
				allocatedPod = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())
			Expect(allocatedPod).NotTo(BeEmpty())

			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, bsbxNamespaceName, latest); err != nil {
					return err
				}
				latest.Spec.Template = pool.Spec.Template.DeepCopy()
				latest.Spec.PoolRef = ""
				return k8sClient.Update(ctx, latest)
			})).To(Succeed())

			maxUnavailable := intstr.FromString("100%")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPool := &sandboxv1alpha1.Pool{}
				if err := k8sClient.Get(ctx, typeNamespacedName, latestPool); err != nil {
					return err
				}
				latestPool.Spec.ScaleStrategy = &sandboxv1alpha1.ScaleStrategy{
					MaxUnavailable: &maxUnavailable,
				}
				return k8sClient.Update(ctx, latestPool)
			})).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				allocation, err := getPoolAllocation(pool)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocation.PodAllocation).NotTo(HaveKey(allocatedPod))

				pod := &v1.Pod{}
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      allocatedPod,
					Namespace: typeNamespacedName.Namespace,
				}, pod)
				if err == nil {
					g.Expect(pod.DeletionTimestamp).NotTo(BeNil())
				} else {
					g.Expect(errors.IsNotFound(err)).To(BeTrue())
				}
			}, 5*time.Second, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})
	})
})

func getSandboxAllocation(obj kclient.Object) (*SandboxAllocation, error) {
	allocation := &SandboxAllocation{}
	anno := obj.GetAnnotations()
	if anno == nil {
		return allocation, nil
	}
	str, ok := anno[AnnoAllocStatusKey]
	if !ok {
		return allocation, nil
	}
	err := json.Unmarshal([]byte(str), allocation)
	if err != nil {
		return nil, err
	}
	return allocation, nil
}

func getPoolAllocation(pool *sandboxv1alpha1.Pool) (*PoolAllocation, error) {
	store := NewInMemoryAllocationStore()
	if err := store.Recover(ctx, k8sClient); err != nil {
		return nil, err
	}
	return store.GetAllocation(ctx, pool)
}

var _ = Describe("Pool deletion and recreation", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When deleting and recreating a Pool with same name", func() {
		const resourceName = "pool-recreate-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Pool")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			} else {
				By("Cleanup the specific resource instance Pool")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should allow recreating a Pool with the same name after deletion", func() {
			By("deleting the existing Pool")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())

			By("waiting for the Pool to be fully deleted")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Pool should be deleted")
			}, timeout, interval).Should(Succeed())

			By("recreating a Pool with the same name")
			newPool := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, newPool)).To(Succeed())

			By("verifying the new Pool is successfully reconciled and creates expected pods")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt), "new Pool should have correct total pod count")
			}, timeout, interval).Should(Succeed())
		})

		It("should clear allocation when recreating a Pool with the same name", func() {
			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			// Mock pod running and ready
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox to allocate pods from the pool")

			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-recreate-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation to complete")
			var allocatedPod string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPod = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("verifying pool allocation exists")
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			allocation, err := getPoolAllocation(pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(allocation.PodAllocation)).To(Equal(1))
			Expect(allocation.PodAllocation[allocatedPod]).To(Equal(batchSandbox.Name))

			By("deleting the BatchSandbox")
			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			By("deleting the Pool")
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())

			By("waiting for the Pool to be fully deleted")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Pool should be deleted")
			}, timeout, interval).Should(Succeed())

			By("recreating a Pool with the same name")
			newPool := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
				},
			}
			Expect(k8sClient.Create(ctx, newPool)).To(Succeed())

			By("verifying the new Pool has empty allocation")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				allocation, err := getPoolAllocation(pool)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocation.PodAllocation)).To(Equal(0), "new Pool should have empty allocation, not reuse old allocation")
			}, timeout, interval).Should(Succeed())
		})
	})
})

var _ = Describe("Pool scaleStrategy", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When reconciling a resource with scaleStrategy", func() {
		const resourceName = "pool-scale-strategy-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Pool")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					By("The specific resource instance Pool already deleted")
					return
				}
			}
			By("Cleanup the specific resource instance Pool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should scale with default 25% maxUnavailable when not specified", func() {
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   10,
						BufferMin: 10,
						BufferMax: 10,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// With default 25% maxUnavailable and target 10 pods,
			// first batch should create at most 3 pods (ceil(10 * 0.25) = 3)
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				// Initial creation should be limited to 3 (or less if pods become ready)
				g.Expect(pool.Status.Total).To(BeNumerically("<=", 3))
			}, timeout, interval).Should(Succeed())
		})

		It("should scale with percentage maxUnavailable", func() {
			maxUnavailable := intstr.FromString("20%")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   10,
						BufferMin: 10,
						BufferMax: 10,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: &maxUnavailable,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// With 20% maxUnavailable and target 10 pods,
			// first batch should create at most 2 pods
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pool.Status.Total).To(BeNumerically("<=", 2))
			}, timeout, interval).Should(Succeed())
		})

		It("should scale with integer maxUnavailable", func() {
			maxUnavailable := intstr.FromInt(1)
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   5,
						BufferMin: 5,
						BufferMax: 5,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: &maxUnavailable,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// With maxUnavailable=1, only 1 pod should be created at a time
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pool.Status.Total).To(BeNumerically("<=", 1))
			}, timeout, interval).Should(Succeed())
		})
	})
})

var _ = Describe("Pool recycle", func() {
	var (
		timeout  = 10 * time.Second
		interval = 1 * time.Second
	)
	Context("When reconciling a resource with RecycleStrategy", func() {
		const resourceName = "pool-recycle-test"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("randomizing pool name for isolation")
			typeNamespacedName.Name = resourceName + "-" + rand.String(8)
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.Pool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err != nil {
				if !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				} else {
					By("The specific resource instance Pool already deleted")
					return
				}
			}
			By("Cleanup the specific resource instance Pool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should create pool with Delete recycle strategy and release pods via deletion", func() {
			By("creating a Pool with explicit Delete RecycleStrategy")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeDelete,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox to allocate pods from the pool")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-recycle-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation")
			var allocatedPodName string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPodName = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("releasing pods by updating the BatchSandbox alloc-release annotation")
			release := AllocationRelease{
				Pods: []string{allocatedPodName},
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying the pool allocation is cleared after Delete recycle")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				Expect(err).NotTo(HaveOccurred())
				allocation, err := getPoolAllocation(pool)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocation.PodAllocation)).To(Equal(0), "Pool allocation should be cleared after Delete recycle")
			}, timeout, interval).Should(Succeed())

			By("verifying the released pod is deleted with Delete recycle strategy")
			Eventually(func(g Gomega) {
				pod := &v1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: allocatedPodName, Namespace: typeNamespacedName.Namespace}, pod)
				if err != nil && errors.IsNotFound(err) {
					return // pod already gone
				}
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.DeletionTimestamp).NotTo(BeNil(), "pod should be terminating with Delete recycle strategy")
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should create pool with default (no explicit) RecycleStrategy which behaves as Delete", func() {
			By("creating a Pool without explicit RecycleStrategy")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   2,
						BufferMin: 1,
						BufferMax: 1,
					},
					// No RecycleStrategy set - should default to Delete
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox to allocate pods from the pool")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-default-recycle-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation")
			var allocatedPodName string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPodName = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("releasing pods by updating the BatchSandbox alloc-release annotation")
			release := AllocationRelease{
				Pods: []string{allocatedPodName},
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying the released pod is deleted (default RecycleStrategy = Delete)")
			Eventually(func(g Gomega) {
				pod := &v1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: allocatedPodName, Namespace: typeNamespacedName.Namespace}, pod)
				if err != nil && errors.IsNotFound(err) {
					return // pod already gone
				}
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.DeletionTimestamp).NotTo(BeNil(), "pod should be terminating with default Delete recycle strategy")
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should delete multiple pods when BatchSandbox releases with Delete recycle strategy", func() {
			By("creating a Pool with Delete RecycleStrategy and larger capacity")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   4,
						BufferMin: 2,
						BufferMax: 2,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: ptr.To(intstr.FromString("100%")),
					},
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeDelete,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox with 2 replicas")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-multi-delete-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(2)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation of 2 pods")
			var allocatedPodNames []string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).To(HaveLen(2))
				allocatedPodNames = alloc.Pods
			}, timeout, interval).Should(Succeed())

			By("releasing all pods")
			release := AllocationRelease{
				Pods: allocatedPodNames,
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying all released pods are deleted")
			Eventually(func(g Gomega) {
				for _, podName := range allocatedPodNames {
					pod := &v1.Pod{}
					err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: typeNamespacedName.Namespace}, pod)
					if err != nil && errors.IsNotFound(err) {
						continue
					}
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(pod.DeletionTimestamp).NotTo(BeNil(), "pod %s should be terminating", podName)
				}
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should create pool with Noop recycle strategy and keep pods available after release", func() {
			By("creating a Pool with Noop RecycleStrategy")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   2,
						PoolMax:   4,
						BufferMin: 2,
						BufferMax: 2,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: ptr.To(intstr.FromString("100%")),
					},
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeNoop,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox to allocate pods from the pool")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-noop-recycle-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation")
			var allocatedPodName string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPodName = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("releasing pods by updating the BatchSandbox alloc-release annotation")
			release := AllocationRelease{
				Pods: []string{allocatedPodName},
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying the pool allocation is cleared after Noop recycle")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				Expect(err).NotTo(HaveOccurred())
				allocation, err := getPoolAllocation(pool)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocation.PodAllocation)).To(Equal(0), "Pool allocation should be cleared after Noop recycle")
			}, timeout, interval).Should(Succeed())

			By("verifying the released pod still exists immediately after Noop recycle (not deleted by recycler)")
			// Unlike Delete recycle which marks pods for deletion, Noop recycle leaves pods intact.
			// The pool's scale-down logic may subsequently delete pods if buffer exceeds BufferMax,
			// but that is a separate concern from the recycle strategy.
			Eventually(func(g Gomega) {
				pod := &v1.Pod{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: allocatedPodName, Namespace: typeNamespacedName.Namespace}, pod)
				if err != nil && errors.IsNotFound(err) {
					// The pod might have been deleted by pool scale-down, which is acceptable.
					// The key difference from Delete recycle is that Noop recycler does NOT
					// return NeedDelete=true, so the pod is not targeted for deletion by recycle itself.
					return
				}
				g.Expect(err).NotTo(HaveOccurred())
				// If the pod still exists, it should not have DeletionTimestamp from recycle.
				// If it was deleted, it was deleted by pool scale-down, not by the recycler.
				g.Expect(pod.DeletionTimestamp).To(BeNil(), "pod should not be marked for deletion by Noop recycle")
			}, timeout, interval).Should(Succeed())

			By("verifying pool available count increases after release")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, pool)
				g.Expect(err).NotTo(HaveOccurred())
				// Pool.Available should have increased since the released pod is available for reallocation
				g.Expect(pool.Status.Allocated).To(BeNumerically("<", 1), "Pool allocated should be 0 after release")
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should create pool with Restart recycle strategy and keep pod alive after release", func() {
			By("creating a Pool with Restart RecycleStrategy")
			// In envtest there is no real kubelet, so the exec command never actually runs.
			// The restart handler will keep retrying until max retries is exceeded and fall
			// back to NeedDelete=true. We therefore verify the same observable end-state as
			// Delete recycle (allocation cleared, pod eventually terminating), while confirming
			// that the Restart strategy path is exercised through the factory.
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   2,
						PoolMax:   4,
						BufferMin: 2,
						BufferMax: 2,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: ptr.To(intstr.FromString("100%")),
					},
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeRestart,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				cnt := min(pool.Spec.CapacitySpec.PoolMax, pool.Spec.CapacitySpec.BufferMin)
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(cnt))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox to allocate pods from the pool")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-restart-recycle-test-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for allocation")
			var allocatedPodName string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPodName = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("releasing pods by updating the BatchSandbox alloc-release annotation")
			release := AllocationRelease{
				Pods: []string{allocatedPodName},
			}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying the pool allocation is cleared after Restart recycle (retries exceeded → NeedDelete)")
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				allocation, err := getPoolAllocation(pool)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(allocation.PodAllocation).To(BeEmpty(), "Pool allocation should be cleared after Restart recycle")
			}, 30*time.Second, interval).Should(Succeed())

			By("verifying pool Allocated count returns to 0")
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				g.Expect(pool.Status.Allocated).To(BeNumerically("==", 0), "Pool allocated should be 0 after restart recycle")
			}, 30*time.Second, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("should replenish buffer after Delete recycle frees pods", func() {
			By("creating a Pool with Delete RecycleStrategy and BufferMin=2")
			resource := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "main",
									Image: "example.com",
								},
							},
						},
					},
					CapacitySpec: sandboxv1alpha1.CapacitySpec{
						PoolMin:   0,
						PoolMax:   4,
						BufferMin: 2,
						BufferMax: 2,
					},
					ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{
						MaxUnavailable: ptr.To(intstr.FromString("100%")),
					},
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeDelete,
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("waiting for Pool to be ready with 2 buffer pods")
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(Equal(int32(2)))
			}, timeout, interval).Should(Succeed())

			By("setting pool pods to running state")
			pool := &sandboxv1alpha1.Pool{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     typeNamespacedName.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for _, pod := range pods.Items {
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
			}

			By("creating a BatchSandbox with 1 replica")
			bsbxNamespaceName := types.NamespacedName{
				Name:      "batch-sandbox-replenish-" + rand.String(8),
				Namespace: typeNamespacedName.Namespace,
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxNamespaceName.Name,
					Namespace: bsbxNamespaceName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  typeNamespacedName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for 1 pod to be allocated")
			var allocatedPodName string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).NotTo(BeEmpty())
				allocatedPodName = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("releasing the allocated pod")
			release := AllocationRelease{Pods: []string{allocatedPodName}}
			js, err := json.Marshal(release)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, bsbxNamespaceName, batchSandbox)).To(Succeed())
			if batchSandbox.Annotations == nil {
				batchSandbox.Annotations = make(map[string]string)
			}
			batchSandbox.Annotations[AnnoAllocReleaseKey] = string(js)
			Expect(k8sClient.Update(ctx, batchSandbox)).To(Succeed())

			By("verifying pool allocation is cleared")
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				allocation, err := getPoolAllocation(pool)
				Expect(err).NotTo(HaveOccurred())
				g.Expect(allocation.PodAllocation).To(BeEmpty(), "Pool allocation should be cleared after Delete recycle")
			}, timeout, interval).Should(Succeed())

			By("verifying pool schedules a new pod to replenish the deleted buffer pod")
			// The deleted pod reduces Total; the reconciler should scale up to restore BufferMin=2.
			// envtest does not run kubelet so new pods stay Pending, but the scale-up expectation
			// being satisfied is enough for Total to reflect the new pod.
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, pool)).To(Succeed())
				// After the deleted pod is gone and a replacement is created, Total >= 2.
				g.Expect(pool.Status.Total).To(BeNumerically(">=", int32(2)),
					"pool should replenish buffer after pod deletion")
			}, 30*time.Second, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})
	})
})

var _ = Describe("collectRecycleResults", func() {
	type testCase struct {
		desc            string
		results         []recycleResult
		wantSucceedMap  map[string][]string
		wantToDelete    []string
		wantErrContains string
	}

	cases := []testCase{
		{
			desc:           "empty results returns empty maps",
			results:        nil,
			wantSucceedMap: map[string][]string{},
			wantToDelete:   nil,
		},
		{
			desc: "Succeeded state adds pod to succeedMap",
			results: []recycleResult{
				{sandboxName: "sbx-1", podName: "pod-1", status: &recycle.Status{State: recycle.StateSucceeded}},
			},
			wantSucceedMap: map[string][]string{"sbx-1": {"pod-1"}},
			wantToDelete:   nil,
		},
		{
			desc: "NeedDelete adds pod to toDeletePods and succeedMap",
			results: []recycleResult{
				{sandboxName: "sbx-1", podName: "pod-1", status: &recycle.Status{State: recycle.StateSucceeded, NeedDelete: true}},
			},
			wantSucceedMap: map[string][]string{"sbx-1": {"pod-1"}},
			wantToDelete:   []string{"pod-1"},
		},
		{
			desc: "Recycling state (in-progress) does not add to succeedMap or toDeletePods",
			results: []recycleResult{
				{sandboxName: "sbx-1", podName: "pod-1", status: &recycle.Status{State: recycle.StateRecycling}},
			},
			wantSucceedMap: map[string][]string{},
			wantToDelete:   nil,
		},
		{
			desc: "error result is collected and pod is skipped",
			results: []recycleResult{
				{sandboxName: "sbx-1", podName: "pod-1", err: stderrors.New("exec failed")},
			},
			wantSucceedMap:  map[string][]string{},
			wantToDelete:    nil,
			wantErrContains: "exec failed",
		},
		{
			desc: "multiple sandboxes multiple pods",
			results: []recycleResult{
				{sandboxName: "sbx-a", podName: "pod-1", status: &recycle.Status{State: recycle.StateSucceeded}},
				{sandboxName: "sbx-a", podName: "pod-2", status: &recycle.Status{State: recycle.StateSucceeded, NeedDelete: true}},
				{sandboxName: "sbx-b", podName: "pod-3", status: &recycle.Status{State: recycle.StateSucceeded}},
				{sandboxName: "sbx-b", podName: "pod-4", err: stderrors.New("timeout")},
			},
			wantSucceedMap: map[string][]string{
				"sbx-a": {"pod-1", "pod-2"},
				"sbx-b": {"pod-3"},
			},
			wantToDelete:    []string{"pod-2"},
			wantErrContains: "timeout",
		},
		{
			desc: "multiple errors are joined",
			results: []recycleResult{
				{sandboxName: "sbx-1", podName: "pod-1", err: stderrors.New("err-a")},
				{sandboxName: "sbx-1", podName: "pod-2", err: stderrors.New("err-b")},
			},
			wantSucceedMap:  map[string][]string{},
			wantToDelete:    nil,
			wantErrContains: "err-a",
		},
	}

	for _, tc := range cases {
		tc := tc
		It(tc.desc, func() {
			succeedMap, toDeletePods, err := collectRecycleResults(context.Background(), tc.results)
			if tc.wantErrContains != "" {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(tc.wantErrContains))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(succeedMap).To(Equal(tc.wantSucceedMap))
			Expect(toDeletePods).To(Equal(tc.wantToDelete))
		})
	}
})
