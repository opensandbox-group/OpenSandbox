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
	gerrors "errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"k8s.io/utils/set"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	taskscheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler"
	mock_scheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler/mock"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func init() {
	testscheme = k8sruntime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(testscheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(testscheme))
}

var testscheme *k8sruntime.Scheme

var _ = Describe("BatchSandbox Controller", func() {
	var (
		timeout  = 30 * time.Second
		interval = 5 * time.Second
	)
	// None Pooling Mode
	Context("When create new batch sandbox, create pod base on pod template", func() {
		const resourceBaseName = "test-batch-sandbox"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceBaseName,
			Namespace: "default",
		}

		BeforeEach(func() {
			typeNamespacedName.Name = fmt.Sprintf("%s-%s", resourceBaseName, rand.String(5))
			By(fmt.Sprintf("creating the custom resource %s for the Kind BatchSandbox", typeNamespacedName))
			resource := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(3)),
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
				},
			}
			Expect(k8sClient.Create(ctx, resource)).Should(Succeed())
			bs := &sandboxv1alpha1.BatchSandbox{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			By(fmt.Sprintf("wait the custom resource %s created", typeNamespacedName))
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.BatchSandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			} else {
				return
			}
			By(fmt.Sprintf("Cleanup the specific resource instance BatchSandbox %s", typeNamespacedName))
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully create pod, update batch sandbox status, endpoints info", func() {
			wantIPSet := make(set.Set[string])
			podIPMap := make(map[string]string)
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				allPods := &corev1.PodList{}
				g.Expect(k8sClient.List(ctx, allPods, &client.ListOptions{Namespace: bs.Namespace})).Should(Succeed())
				pods := []*corev1.Pod{}
				for i := range allPods.Items {
					po := &allPods.Items[i]
					if metav1.IsControlledBy(po, bs) {
						pods = append(pods, po)
						if po.Status.PodIP != "" {
							continue
						}
						if i%2 == 0 {
							mockIP := randomIPv4().String()
							wantIPSet.Insert(mockIP)
							podIPMap[po.Name] = mockIP
							po.Status.PodIP = mockIP
							po.Status.Phase = corev1.PodRunning
							po.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
							Expect(k8sClient.Status().Update(context.Background(), po)).To(Succeed())
						}
					}
				}
				g.Expect(len(pods)).To(Equal(int(*bs.Spec.Replicas)))
				g.Expect(bs.Status.ObservedGeneration).To(Equal(bs.Generation))
				g.Expect(bs.Status.Replicas).To(Equal(*bs.Spec.Replicas))

				gotIPs := []string{}
				if raw := bs.Annotations[AnnotationSandboxEndpoints]; raw != "" {
					json.Unmarshal([]byte(raw), &gotIPs)
				}

				podIndex, err := calPodIndex(strategy.NewPoolStrategy(bs), bs, pods)
				g.Expect(err).NotTo(HaveOccurred())
				expectedIPs := make([]string, len(pods))
				for _, pod := range pods {
					idx, ok := podIndex[pod.Name]
					g.Expect(ok).To(BeTrue(), fmt.Sprintf("pod %s should have index", pod.Name))
					if pod.Status.PodIP != "" {
						expectedIPs[idx] = pod.Status.PodIP
					} else {
						expectedIPs[idx] = ""
					}
				}
				g.Expect(gotIPs).To(Equal(expectedIPs), "endpoints should be ordered by pod index, unassigned pods should have empty string")
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully correctly create new Pod and update batch sandbox status when user scale out", func() {
			Expect(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return err
				}
				*bs.Spec.Replicas = *bs.Spec.Replicas + 1 // scale out using the latest resourceVersion
				return k8sClient.Update(ctx, bs)
			})).Should(Succeed())
			Eventually(func(g Gomega) {
				batchsandbox := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, batchsandbox); err != nil {
					return
				}
				g.Expect(batchsandbox.Status.ObservedGeneration).To(Equal(batchsandbox.Generation))
				g.Expect(batchsandbox.Status.Replicas).To(Equal(*batchsandbox.Spec.Replicas))
			}, timeout, interval).Should(Succeed())
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
				pods := &v1.PodList{}
				g.Expect(k8sClient.List(ctx, pods, &client.ListOptions{
					Namespace:     bs.Namespace,
					FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(bs.UID)}),
				})).Should(Succeed())
				g.Expect(int32(len(pods.Items))).To(Equal(*bs.Spec.Replicas))
			}, timeout, interval).Should(Succeed())
		})
		It("should successfully correctly supply Pod when pod is deleted unexpectedly", func() {
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				g.Expect(bs.Status.ObservedGeneration).To(Equal(bs.Generation))
				g.Expect(bs.Status.Replicas).To(Equal(*bs.Spec.Replicas))
			}, timeout, interval).Should(Succeed())
			bs := &sandboxv1alpha1.BatchSandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).Should(Succeed())
			pods := &v1.PodList{}
			Expect(k8sClient.List(ctx, pods, &client.ListOptions{
				Namespace:     bs.Namespace,
				FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(bs.UID)}),
			})).Should(Succeed())
			Expect(int32(len(pods.Items))).To(Equal(*bs.Spec.Replicas))
			// delete first pod
			oldPod := pods.Items[0]
			Expect(k8sClient.Delete(ctx, &oldPod)).Should(Succeed())
			// wait supply pod
			Eventually(func(g Gomega) {
				newPod := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Namespace: bs.Namespace,
					Name:      oldPod.Name,
				}, newPod); err != nil {
					return
				}
				g.Expect(newPod.CreationTimestamp).NotTo(Equal(oldPod.CreationTimestamp))
			}, timeout, interval).Should(Succeed())
		})
		It("should delete batch sandbox and related Pods for expired batch sandbox", func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return err
				}
				bs.Spec.ExpireTime = &metav1.Time{Time: time.Now().Add(3 * time.Second)}
				return k8sClient.Update(ctx, bs)
			})).Should(Succeed())

			Eventually(
				func(g Gomega) {
					bs := &sandboxv1alpha1.BatchSandbox{}
					g.Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, bs))).To(BeTrue())
					allPods := &corev1.PodList{}
					g.Expect(k8sClient.List(ctx, allPods, &client.ListOptions{Namespace: bs.Namespace})).Should(Succeed())
					pods := []*corev1.Pod{}
					for i := range allPods.Items {
						po := &allPods.Items[i]
						if metav1.IsControlledBy(po, bs) {
							pods = append(pods, po)
						}
					}
					g.Expect(len(pods)).To(BeZero())
				},
				timeout, interval).Should(Succeed())
		})
	})

	// None Pooling Mode - Heterogeneous Pods
	Context("When create new batch sandbox with ShardPatches, create heterogeneous pods", func() {
		const resourceBaseName = "test-batch-sandbox-shard"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceBaseName,
			Namespace: "default",
		}

		BeforeEach(func() {
			typeNamespacedName.Name = fmt.Sprintf("%s-%s", resourceBaseName, rand.String(5))
			By(fmt.Sprintf("creating the custom resource %s for the Kind BatchSandbox with ShardPatches", typeNamespacedName))
			resource := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(3)),
					Template: &v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:    "main",
									Image:   "example.com",
									Command: []string{"default-command"},
								},
							},
						},
					},
					ShardPatches: []runtime.RawExtension{
						{
							Raw: []byte(`{"spec":{"containers":[{"name":"main","command":["custom-command-0"]}]}}`),
						},
						{
							Raw: []byte(`{"spec":{"containers":[{"name":"main","command":["custom-command-1"]}]}}`),
						},
						{
							Raw: []byte(`{"spec":{"containers":[{"name":"main","command":["custom-command-2"]}]}}`),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).Should(Succeed())
			bs := &sandboxv1alpha1.BatchSandbox{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			By(fmt.Sprintf("wait the custom resource %s created", typeNamespacedName))
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.BatchSandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			} else {
				return
			}
			By(fmt.Sprintf("Cleanup the specific resource instance BatchSandbox %s", typeNamespacedName))
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully create heterogeneous pods with different commands", func() {
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				allPods := &corev1.PodList{}
				g.Expect(k8sClient.List(ctx, allPods, &client.ListOptions{Namespace: bs.Namespace})).Should(Succeed())
				pods := []*corev1.Pod{}
				for i := range allPods.Items {
					po := &allPods.Items[i]
					if metav1.IsControlledBy(po, bs) {
						pods = append(pods, po)
					}
				}
				g.Expect(len(pods)).To(Equal(int(*bs.Spec.Replicas)))

				// Verify each pod has the correct patched command
				for _, pod := range pods {
					indexLabel := pod.Labels[LabelBatchSandboxPodIndexKey]
					g.Expect(indexLabel).NotTo(BeEmpty())
					idx, err := strconv.Atoi(indexLabel)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(idx).To(BeNumerically(">=", 0))
					g.Expect(idx).To(BeNumerically("<", int(*bs.Spec.Replicas)))

					// Verify the command was patched
					g.Expect(len(pod.Spec.Containers)).To(BeNumerically(">", 0))
					mainContainer := pod.Spec.Containers[0]
					expectedCommand := fmt.Sprintf("custom-command-%d", idx)
					g.Expect(mainContainer.Command).To(Equal([]string{expectedCommand}))
				}

				g.Expect(bs.Status.ObservedGeneration).To(Equal(bs.Generation))
				g.Expect(bs.Status.Replicas).To(Equal(*bs.Spec.Replicas))
			}, timeout, interval).Should(Succeed())
		})
	})

	// Pooling Mode
	Context("When create new batch sandbox, get pod from pool", func() {
		const resourceBaseName = "test-batch-sandbox-pooling-mode"
		var replicas int32 = 3
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceBaseName,
			Namespace: "default",
		}
		BeforeEach(func() {
			typeNamespacedName.Name = fmt.Sprintf("%s-%s", resourceBaseName, rand.String(5))
			By(fmt.Sprintf("creating the custom resource %s for the Kind BatchSandbox", typeNamespacedName))
			resource := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(replicas),
					PoolRef:  "test-pool",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).Should(Succeed())
			bs := &sandboxv1alpha1.BatchSandbox{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			By(fmt.Sprintf("wait the custom resource %s created", typeNamespacedName))
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.BatchSandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			By(fmt.Sprintf("Cleanup the specific resource instance BatchSandbox %s", typeNamespacedName))
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully update batch sandbox status, sbx endpoints info when get pod from pool alloc", func() {
			// mock pool allocation
			mockPods := []string{}
			for i := range replicas {
				po := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: typeNamespacedName.Namespace,
						Name:      fmt.Sprintf("test-pod-%d", i),
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "main", Image: "test", Command: []string{"hello"}},
						},
					},
				}
				mockPods = append(mockPods, po.Name)
				Expect(k8sClient.Create(context.Background(), po)).To(Succeed())
				if i%2 == 0 {
					po.Spec.NodeName = "node-1.2.3.4"
					po.Status.PodIP = fmt.Sprintf("1.2.3.%d", i+1)
					po.Status.Phase = corev1.PodRunning
					po.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				}
				Expect(k8sClient.Status().Update(context.Background(), po)).To(Succeed())
			}
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return err
				}
				setSandboxAllocation(bs, SandboxAllocation{Pods: mockPods})
				return k8sClient.Update(ctx, bs)
			})).Should(Succeed())
			By(fmt.Sprintf("Mock pool allocate Pod %v for BatchSandbox %s", mockPods, typeNamespacedName))

			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				g.Expect(bs.Status.ObservedGeneration).To(Equal(bs.Generation))
				g.Expect(bs.Status.Replicas).To(Equal(*bs.Spec.Replicas))

				gotIPs := []string{}
				if raw := bs.Annotations[AnnotationSandboxEndpoints]; raw != "" {
					json.Unmarshal([]byte(raw), &gotIPs)
				}

				alloc, err := parseSandboxAllocation(bs)
				g.Expect(err).NotTo(HaveOccurred())
				expectedIPs := make([]string, len(alloc.Pods))
				for idx, podName := range alloc.Pods {
					pod := &corev1.Pod{}
					err := k8sClient.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: podName}, pod)
					g.Expect(err).NotTo(HaveOccurred())
					if pod.Spec.NodeName != "" || pod.Status.PodIP != "" {
						expectedIPs[idx] = pod.Status.PodIP
					} else {
						expectedIPs[idx] = ""
					}
				}
				g.Expect(gotIPs).To(Equal(expectedIPs), "endpoints should be ordered by pool allocation order, unassigned pods should have empty string")
			}, timeout, interval).Should(Succeed())
		})
	})
})

func randomIPv4() net.IP {
	rand.Seed(time.Now().UnixNano())
	ip := make(net.IP, 4)
	for i := range ip {
		ip[i] = byte(rand.Intn(256))
	}
	return ip
}

var _ = Describe("BatchSandbox Task Scheduler", func() {
	var (
		timeout  = 30 * time.Second
		interval = 5 * time.Second
	)
	// None Pooling mode
	Context("When create new batch sandbox, create pod base on pod template", func() {
		const resourceBaseName = "test-task-batch-sandbox"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceBaseName,
			Namespace: "default",
		}

		BeforeEach(func() {
			typeNamespacedName.Name = fmt.Sprintf("%s-%s", resourceBaseName, rand.String(5))
			By(fmt.Sprintf("creating the custom resource %s for the Kind BatchSandbox", typeNamespacedName))
			resource := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      typeNamespacedName.Name,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(1)),
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
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
						Spec: sandboxv1alpha1.TaskSpec{
							Process: &sandboxv1alpha1.ProcessTask{
								Command: []string{"echo", "hello"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).Should(Succeed())
			bs := &sandboxv1alpha1.BatchSandbox{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			By(fmt.Sprintf("wait the custom resource %s created", typeNamespacedName))
		})

		AfterEach(func() {
			resource := &sandboxv1alpha1.BatchSandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			} else {
				// resource is already deleted
				return
			}
			By(fmt.Sprintf("Cleanup the specific resource instance BatchSandbox %s", typeNamespacedName))
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully add task cleanup finalizer", func() {
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				g.Expect(controllerutil.ContainsFinalizer(bs, FinalizerTaskCleanup)).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should successfully update task status(task_pending=1), because all pods is unassigned", func() {
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				if err := k8sClient.Get(ctx, typeNamespacedName, bs); err != nil {
					return
				}
				g.Expect(bs.Status.ObservedGeneration).To(Equal(bs.Generation))
				g.Expect(bs.Status.Replicas).To(Equal(*bs.Spec.Replicas))

				g.Expect(bs.Status.TaskPending).To(Equal(*bs.Spec.Replicas))
				g.Expect(bs.Status.TaskRunning).To(Equal(int32(0)))
				g.Expect(bs.Status.TaskSucceed).To(Equal(int32(0)))
				g.Expect(bs.Status.TaskFailed).To(Equal(int32(0)))
				g.Expect(bs.Status.TaskUnknown).To(Equal(int32(0)))
			}, timeout, interval).Should(Succeed())
		})

		It("should successfully delete BatchSandbox when all tasks(including pending task) cleanup is finished", func() {
			bs := &sandboxv1alpha1.BatchSandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, bs)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(bs, FinalizerTaskCleanup)).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			By(fmt.Sprintf("try to Delete BatchSandbox %s", typeNamespacedName))
			Expect(k8sClient.Delete(ctx, bs)).To(Succeed())

			Eventually(func(g Gomega) {
				bs := &sandboxv1alpha1.BatchSandbox{}
				err := k8sClient.Get(ctx, typeNamespacedName, bs)
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})
	})
})

func TestBatchSandboxReconciler_scheduleTasks(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	var (
		fakeBatchSandbox = &sandboxv1alpha1.BatchSandbox{
			TypeMeta: metav1.TypeMeta{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "BatchSandbox",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-batch-sandbox",
			},
			Spec:   sandboxv1alpha1.BatchSandboxSpec{},
			Status: sandboxv1alpha1.BatchSandboxStatus{},
		}
	)
	type fields struct {
		Client         client.Client
		Scheme         *runtime.Scheme
		Recorder       record.EventRecorder
		taskSchedulers sync.Map
	}
	type args struct {
		ctx      context.Context
		tSch     taskscheduler.TaskScheduler
		batchSbx *sandboxv1alpha1.BatchSandbox
	}
	tests := []struct {
		name                string
		fields              fields
		args                args
		wantErr             bool
		wantTaskStatus      *taskScheduleResult
		batchSandboxChecker func(bsbx *sandboxv1alpha1.BatchSandbox) error
	}{
		{
			name: "schedule err",
			args: args{
				tSch: func() taskscheduler.TaskScheduler {
					mockSche := mock_scheduler.NewMockTaskScheduler(ctrl)
					mockSche.EXPECT().Schedule().Return(gerrors.New("err")).Times(1)
					return mockSche
				}(),
			},
			wantErr: true,
		},
		{
			name: "tasks, succeed=1; releasedPod=1",
			fields: fields{
				Client: fake.NewClientBuilder().WithScheme(testscheme).WithObjects(fakeBatchSandbox).WithStatusSubresource(fakeBatchSandbox).Build(),
			},
			args: args{
				tSch: func() taskscheduler.TaskScheduler {
					mockSche := mock_scheduler.NewMockTaskScheduler(ctrl)
					mockSche.EXPECT().Schedule().Return(nil).Times(1)
					mockTask := mock_scheduler.NewMockTask(ctrl)
					mockTask.EXPECT().GetState().Return(taskscheduler.SucceedTaskState).Times(1)
					mockTask.EXPECT().IsResourceReleased().Return(true).Times(1)
					mockTask.EXPECT().GetPodName().Return("pod-0").AnyTimes()
					mockSche.EXPECT().ListTask().Return([]taskscheduler.Task{mockTask}).Times(1)
					return mockSche
				}(),
				batchSbx: fakeBatchSandbox.DeepCopy(),
			},
			wantTaskStatus: &taskScheduleResult{Succeed: 1},
			batchSandboxChecker: func(bsbx *sandboxv1alpha1.BatchSandbox) error {
				release, err := parseSandboxReleased(bsbx)
				if err != nil {
					return err
				}
				if len(release.Pods) != 1 || release.Pods[0] != "pod-0" {
					return fmt.Errorf("expect pod-0, actual %v", release.Pods)
				}
				return nil
			},
		},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			r := &BatchSandboxReconciler{
				Client:   tt.fields.Client,
				Scheme:   tt.fields.Scheme,
				Recorder: tt.fields.Recorder,
			}
			gotTaskStatus, err := r.scheduleTasks(tt.args.ctx, tt.args.tSch, tt.args.batchSbx)
			if (err != nil) != tt.wantErr {
				t.Errorf("BatchSandboxReconciler.scheduleTasks() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(gotTaskStatus, tt.wantTaskStatus) {
				t.Errorf("BatchSandboxReconciler.scheduleTasks() = %v, want %v", gotTaskStatus, tt.wantTaskStatus)
			}
			if tt.wantErr && gotTaskStatus != nil {
				t.Errorf("BatchSandboxReconciler.scheduleTasks() = %v, want nil when error", gotTaskStatus)
			}
			if tt.batchSandboxChecker != nil {
				bsbx := &sandboxv1alpha1.BatchSandbox{}
				if err := tt.fields.Client.Get(ctx, types.NamespacedName{Namespace: tt.args.batchSbx.Namespace, Name: tt.args.batchSbx.Name}, bsbx); err != nil {
					t.Errorf("BatchSandboxReconciler Get() error = %v, wantErr %v", err, nil)
				}
				if err := tt.batchSandboxChecker(bsbx); err != nil {
					t.Errorf("BatchSandboxReconciler batchSandboxChecker() error = %v, wantErr %v", err, nil)
				}
			}
		})
	}
}

func TestBatchSandboxReconciler_reconcileTasks(t *testing.T) {
	ctx := context.Background()

	type fields struct {
		Client   client.Client
		Scheme   *runtime.Scheme
		Recorder record.EventRecorder
	}
	type args struct {
		ctx      context.Context
		batchSbx *sandboxv1alpha1.BatchSandbox
		pods     []*corev1.Pod
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
		checker func(r *BatchSandboxReconciler, batchSbx *sandboxv1alpha1.BatchSandbox) error
	}{
		{
			name: "deletion with finalizer cleanup success",
			fields: func() fields {
				return fields{
					Client: fake.NewClientBuilder().WithScheme(testscheme).Build(),
				}
			}(),
			args: func() args {
				return args{
					ctx:  ctx,
					pods: []*corev1.Pod{},
				}
			}(),
			wantErr: false,
			checker: func(r *BatchSandboxReconciler, batchSbx *sandboxv1alpha1.BatchSandbox) error {
				// Verify finalizer is removed by getting from client
				updated := &sandboxv1alpha1.BatchSandbox{}
				if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}, updated); err != nil {
					return fmt.Errorf("failed to get updated batchsandbox: %w", err)
				}
				if controllerutil.ContainsFinalizer(updated, FinalizerTaskCleanup) {
					return fmt.Errorf("finalizer should be removed after cleanup, finalizers: %v", updated.Finalizers)
				}
				return nil
			},
		},
	}

	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			r := &BatchSandboxReconciler{
				Client:   tt.fields.Client,
				Scheme:   tt.fields.Scheme,
				Recorder: tt.fields.Recorder,
			}

			// Setup: create BatchSandbox with finalizer
			bsbx := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-batch-cleanup",
					Namespace:  "default",
					UID:        types.UID("test-uid-cleanup"),
					Finalizers: []string{FinalizerTaskCleanup},
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Replicas: ptr.To(int32(0)),
				},
			}

			if err := r.Client.Create(ctx, bsbx); err != nil {
				t.Fatalf("failed to create batchsandbox: %v", err)
			}

			// Set deletion timestamp after creation (fake client doesn't preserve it during Create)
			now := metav1.Now()
			bsbx.DeletionTimestamp = &now

			got, err := r.reconcileTasks(tt.args.ctx, bsbx, tt.args.pods)
			if (err != nil) != tt.wantErr {
				t.Errorf("BatchSandboxReconciler.reconcileTasks() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got == nil && tt.checker == nil {
				t.Errorf("BatchSandboxReconciler.reconcileTasks() = nil, want non-nil")
			}
			if tt.checker != nil {
				if err := tt.checker(r, bsbx); err != nil {
					t.Errorf("BatchSandboxReconciler checker() error = %v", err)
				}
			}
		})
	}
}

func Test_parseIndex(t *testing.T) {
	type args struct {
		pod *corev1.Pod
	}
	tests := []struct {
		name    string
		args    args
		want    int
		wantErr bool
	}{
		{
			name: "from label",
			args: args{
				pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelBatchSandboxPodIndexKey: "1"},
					Name: "sbx-0"}},
			},
			want: 1,
		},
		{
			name: "from name",
			args: args{
				pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sbx-0"}},
			},
			want: 0,
		},
		{
			name: "invalid name",
			args: args{
				pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sbx"}},
			},
			want:    -1,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIndex(tt.args.pod)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseIndex() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_calPodIndex(t *testing.T) {
	type args struct {
		batchSbx *sandboxv1alpha1.BatchSandbox
		pods     []*corev1.Pod
	}
	tests := []struct {
		name    string
		args    args
		want    map[string]int
		wantErr bool
	}{
		{
			name: "pool mode - valid allocation",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
						Annotations: map[string]string{
							AnnoAllocStatusKey: `{"pods":["pod-0","pod-1","pod-2"]}`,
						},
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						PoolRef: "test-pool",
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-2"}},
				},
			},
			want: map[string]int{
				"pod-0": 0,
				"pod-1": 1,
				"pod-2": 2,
			},
			wantErr: false,
		},
		{
			name: "pool mode - allocation annotation missing",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						PoolRef: "test-pool",
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}},
				},
			},
			want:    map[string]int{},
			wantErr: false,
		},
		{
			name: "pool mode - invalid allocation json",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
						Annotations: map[string]string{
							AnnoAllocStatusKey: `invalid-json`,
						},
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						PoolRef: "test-pool",
					},
				},
				pods: []*corev1.Pod{},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "pool mode - pods not in allocation list",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
						Annotations: map[string]string{
							AnnoAllocStatusKey: `{"pods":["pod-0","pod-1"]}`,
						},
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						PoolRef: "test-pool",
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod-2"}},
				},
			},
			want: map[string]int{
				"pod-0": 0,
				"pod-1": 1,
			},
			wantErr: false,
		},
		{
			name: "non-pool mode - parse from pod labels",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Replicas: ptr.To(int32(3)),
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{
						Name:   "test-batch-0",
						Labels: map[string]string{LabelBatchSandboxPodIndexKey: "0"},
					}},
					{ObjectMeta: metav1.ObjectMeta{
						Name:   "test-batch-1",
						Labels: map[string]string{LabelBatchSandboxPodIndexKey: "1"},
					}},
					{ObjectMeta: metav1.ObjectMeta{
						Name:   "test-batch-2",
						Labels: map[string]string{LabelBatchSandboxPodIndexKey: "2"},
					}},
				},
			},
			want: map[string]int{
				"test-batch-0": 0,
				"test-batch-1": 1,
				"test-batch-2": 2,
			},
			wantErr: false,
		},
		{
			name: "non-pool mode - parse from pod names",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Replicas: ptr.To(int32(3)),
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "test-batch-0"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "test-batch-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "test-batch-2"}},
				},
			},
			want: map[string]int{
				"test-batch-0": 0,
				"test-batch-1": 1,
				"test-batch-2": 2,
			},
			wantErr: false,
		},
		{
			name: "non-pool mode - invalid pod name",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Replicas: ptr.To(int32(1)),
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "invalid-name-no-index"}},
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "non-pool mode - empty pods list",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Replicas: ptr.To(int32(0)),
					},
				},
				pods: []*corev1.Pod{},
			},
			want:    map[string]int{},
			wantErr: false,
		},
		{
			name: "non-pool mode - mixed label and name parsing",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-batch",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Replicas: ptr.To(int32(3)),
					},
				},
				pods: []*corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{
						Name:   "test-batch-0",
						Labels: map[string]string{LabelBatchSandboxPodIndexKey: "5"},
					}},
					{ObjectMeta: metav1.ObjectMeta{Name: "test-batch-1"}},
				},
			},
			want: map[string]int{
				"test-batch-0": 5,
				"test-batch-1": 1,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poolStrategy := strategy.NewPoolStrategy(tt.args.batchSbx)
			got, err := calPodIndex(poolStrategy, tt.args.batchSbx, tt.args.pods)
			if (err != nil) != tt.wantErr {
				t.Errorf("calPodIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("calPodIndex() = %v, want %v", got, tt.want)
			}
		})
	}
}
