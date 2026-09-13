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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// Regression tests for the spec.poolRef CEL transition rule. Re-pointing a
// bound BatchSandbox to a different pool used to make the previous pool
// recycle the in-use pod while the stale allocation record blocked the new
// pool from supplying a replacement, leaving the sandbox permanently starved.
var _ = Describe("BatchSandbox poolRef immutability", func() {
	var (
		timeout  = 15 * time.Second
		interval = 1 * time.Second
	)

	Context("when a bound BatchSandbox attempts to re-point its poolRef", func() {
		var (
			poolAName types.NamespacedName
			poolBName types.NamespacedName
		)

		newPool := func(name types.NamespacedName) *sandboxv1alpha1.Pool {
			return &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name.Name,
					Namespace: name.Namespace,
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
		}

		markPoolPodsReady := func(g Gomega, name types.NamespacedName) {
			pool := &sandboxv1alpha1.Pool{}
			g.Expect(k8sClient.Get(ctx, name, pool)).To(Succeed())
			pods := &v1.PodList{}
			g.Expect(k8sClient.List(ctx, pods, &kclient.ListOptions{
				Namespace:     name.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
			})).To(Succeed())
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodRunning {
					continue
				}
				pod.Status.Phase = v1.PodRunning
				pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
				g.Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			}
		}

		waitPoolReady := func(name types.NamespacedName) {
			Eventually(func(g Gomega) {
				pool := &sandboxv1alpha1.Pool{}
				g.Expect(k8sClient.Get(ctx, name, pool)).To(Succeed())
				g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
				g.Expect(pool.Status.Total).To(BeNumerically(">=", 1))
				markPoolPodsReady(g, name)
			}, timeout, interval).Should(Succeed())
		}

		BeforeEach(func() {
			poolAName = types.NamespacedName{Name: "immu-pool-a-" + rand.String(8), Namespace: "default"}
			poolBName = types.NamespacedName{Name: "immu-pool-b-" + rand.String(8), Namespace: "default"}
			Expect(k8sClient.Create(ctx, newPool(poolAName))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPool(poolBName))).To(Succeed())
			waitPoolReady(poolAName)
			waitPoolReady(poolBName)
		})

		AfterEach(func() {
			for _, name := range []types.NamespacedName{poolAName, poolBName} {
				pool := &sandboxv1alpha1.Pool{}
				if err := k8sClient.Get(ctx, name, pool); err == nil {
					Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
				}
			}
		})

		It("rejects the re-point and keeps the existing allocation intact", func() {
			bsbxName := types.NamespacedName{
				Name:      "immu-switch-" + rand.String(8),
				Namespace: "default",
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxName.Name,
					Namespace: bsbxName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
					PoolRef:  poolAName.Name,
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("waiting for pool A to allocate a pod to the sandbox")
			var allocatedPod string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, bsbxName, batchSandbox)).To(Succeed())
				alloc, err := getSandboxAllocation(batchSandbox)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).To(HaveLen(1))
				allocatedPod = alloc.Pods[0]
			}, timeout, interval).Should(Succeed())

			By("attempting to re-point spec.poolRef from pool A to pool B")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, bsbxName, latest); err != nil {
					return err
				}
				latest.Spec.PoolRef = poolBName.Name
				return k8sClient.Update(ctx, latest)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be re-pointed"))

			By("verifying the sandbox keeps its pod and binding")
			Consistently(func(g Gomega) {
				pod := &v1.Pod{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      allocatedPod,
					Namespace: bsbxName.Namespace,
				}, pod)).To(Succeed())
				g.Expect(pod.DeletionTimestamp).To(BeNil())

				g.Expect(k8sClient.Get(ctx, bsbxName, batchSandbox)).To(Succeed())
				g.Expect(batchSandbox.Spec.PoolRef).To(Equal(poolAName.Name))
				alloc, err := getSandboxAllocation(batchSandbox)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).To(ConsistOf(allocatedPod))
			}, 5*time.Second, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})

		It("still allows binding an unbound sandbox once and detaching afterwards", func() {
			bsbxName := types.NamespacedName{
				Name:      "immu-bind-" + rand.String(8),
				Namespace: "default",
			}
			batchSandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bsbxName.Name,
					Namespace: bsbxName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(0)),
				},
			}
			Expect(k8sClient.Create(ctx, batchSandbox)).To(Succeed())

			By("binding the unbound sandbox to pool A is allowed")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, bsbxName, latest); err != nil {
					return err
				}
				latest.Spec.PoolRef = poolAName.Name
				return k8sClient.Update(ctx, latest)
			})).To(Succeed())

			By("re-pointing the now-bound sandbox to pool B is rejected")
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, bsbxName, latest); err != nil {
					return err
				}
				latest.Spec.PoolRef = poolBName.Name
				return k8sClient.Update(ctx, latest)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be re-pointed"))

			By("clearing poolRef to detach is allowed")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, bsbxName, latest); err != nil {
					return err
				}
				latest.Spec.PoolRef = ""
				return k8sClient.Update(ctx, latest)
			})).To(Succeed())

			Expect(k8sClient.Delete(ctx, batchSandbox)).To(Succeed())
		})
	})
})
