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

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

var (
	pauseResumeNamespace = utils.PauseResumeNamespace()
	registryServiceAddr  = utils.PauseResumeRegistryAddr()
	registryUsername     = utils.PauseResumeRegistryUser()
	registryPassword     = utils.PauseResumeRegistryPass()
)

var _ = Describe("PauseResume", Ordered, Label("PauseResume"), func() {
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		if err != nil {
			Expect(err.Error()).To(ContainSubstring("AlreadyExists"))
		}

		if pauseResumeNamespace != namespace {
			By("creating pause-resume namespace")
			cmd = exec.Command("kubectl", "create", "ns", pauseResumeNamespace)
			_, err = utils.Run(cmd)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("AlreadyExists"))
			}
		}

		if psa := utils.PodSecurityEnforce(); psa != "" {
			By("labeling the namespace to enforce the " + psa + " security policy")
			cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
				"pod-security.kubernetes.io/enforce="+psa)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with "+psa+" policy")
		} else {
			By("skipping pod-security label (E2E_POD_SECURITY_ENFORCE is empty)")
		}

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy",
			fmt.Sprintf("CONTROLLER_IMG=%s", utils.ControllerImage),
			fmt.Sprintf("SNAPSHOT_REGISTRY=%s", registryServiceAddr))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("waiting for controller to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, 2*time.Minute).Should(Succeed())

		By("creating registry authentication secrets")
		err = createHtpasswdSecret(pauseResumeNamespace)
		Expect(err).NotTo(HaveOccurred())

		err = createDockerRegistrySecrets(pauseResumeNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("deploying Docker Registry")
		registryYAML, err := renderTemplate("testdata/registry-deployment.yaml", map[string]interface{}{
			"RegistryImage": utils.RegistrySourceImage(),
			"Namespace":     pauseResumeNamespace,
		})
		Expect(err).NotTo(HaveOccurred())

		registryFile := filepath.Join("/tmp", "test-registry.yaml")
		err = os.WriteFile(registryFile, []byte(registryYAML), 0644)
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(registryFile)

		cmd = exec.Command("kubectl", "apply", "-f", registryFile)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for registry to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "deployment", "docker-registry",
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.availableReplicas}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("1"))
		}, 2*time.Minute).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up Docker Registry")
		cmd := exec.Command("kubectl", "delete", "deployment", "docker-registry", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "service", "docker-registry", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("cleaning up secrets")
		for _, secret := range []string{"registry-auth", "registry-snapshot-push-secret", "registry-pull-secret"} {
			cmd = exec.Command("kubectl", "delete", "secret", secret, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		}

		By("cleaning up any remaining sandboxsnapshots")
		cmd = exec.Command("kubectl", "delete", "sandboxsnapshots", "--all", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("cleaning up any remaining batchsandboxes")
		cmd = exec.Command("kubectl", "delete", "batchsandboxes", "--all", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
	})

	Context("Pause and Resume", func() {
		It("should complete the full pause-resume flow via spec.pause trigger", func() {
			const sandboxName = "test-pause-resume"

			// --- Step 1: Create BatchSandbox ---
			By("creating BatchSandbox with template")
			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"SandboxImage":     utils.SandboxImage,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pause-resume-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to reach the steady Succeed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			// --- Step 2: Write marker file for rootfs verification ---
			By("getting pod name for marker file")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace, "-o", "json")
			podsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var podList struct {
				Items []struct {
					Metadata struct {
						Name            string `json:"name"`
						OwnerReferences []struct {
							Kind string `json:"kind"`
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(podsJSON), &podList)
			Expect(err).NotTo(HaveOccurred())

			var podName string
			for _, pod := range podList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == sandboxName {
						podName = pod.Metadata.Name
						break
					}
				}
				if podName != "" {
					break
				}
			}
			Expect(podName).NotTo(BeEmpty(), "Should find a pod owned by BatchSandbox")

			markerValue := fmt.Sprintf("pause-test-%d", time.Now().UnixNano())
			By("writing marker file into container for rootfs verification")
			cmd = exec.Command("kubectl", "exec", podName, "-n", pauseResumeNamespace,
				"-c", "sandbox-container", "--", "sh", "-c", fmt.Sprintf("printf '%s' > /tmp/pause-marker", markerValue))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// --- Step 3: Pause - patch spec.pause=true ---
			By("triggering pause by patching spec.pause=true")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox phase to be Paused (snapshot ready, pods deleted)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Paused"))
			}, 3*time.Minute).Should(Succeed())

			// Verify pods are deleted
			By("verifying pods are deleted after pause")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace,
				"-l", "batchsandbox-name="+sandboxName, "-o", "name")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "Pods should be deleted after pause")

			By("verifying the reserved internal SandboxSnapshot exists after pause")
			cmd = exec.Command("kubectl", "get", "sandboxsnapshot", sandboxName+"-pause",
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("Succeed"), "Internal pause snapshot should be ready after pause")

			// --- Step 4: Resume - patch spec.pause=false ---
			By("triggering resume by patching spec.pause=false")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for resumed BatchSandbox to return to Succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying the reserved internal SandboxSnapshot is deleted after successful resume")
			cmd = exec.Command("kubectl", "get", "sandboxsnapshot", sandboxName+"-pause",
				"-n", pauseResumeNamespace, "-o", "name")
			output, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Internal pause snapshot should be deleted after successful resume")

			// --- Step 5: Verify rootfs data persistence ---
			By("getting resumed pod name")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace, "-o", "json")
			resumedPodsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodList struct {
				Items []struct {
					Metadata struct {
						Name            string `json:"name"`
						OwnerReferences []struct {
							Kind string `json:"kind"`
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(resumedPodsJSON), &resumedPodList)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodName string
			for _, pod := range resumedPodList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == sandboxName {
						resumedPodName = pod.Metadata.Name
						break
					}
				}
				if resumedPodName != "" {
					break
				}
			}
			Expect(resumedPodName).NotTo(BeEmpty(), "Should find a pod owned by resumed BatchSandbox")

			By("reading marker file from resumed container to verify rootfs persistence")
			cmd = exec.Command("kubectl", "exec", resumedPodName, "-n", pauseResumeNamespace,
				"-c", "sandbox-container", "--", "cat", "/tmp/pause-marker")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(markerValue), "Rootfs data should persist across pause/resume")

			// --- Cleanup ---
			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})

		It("should complete pool-based pause-resume via spec.pause trigger", func() {
			const poolName = "test-pool-pause"
			const sandboxName = "test-pool-pause-resume"

			// --- Step 1: Create Pool CR ---
			By("creating Pool CR")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"Namespace":    pauseResumeNamespace,
				"SandboxImage": utils.SandboxImage,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      5,
				"PoolMin":      1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-pause.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to have available pods")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.available}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				g.Expect(output).NotTo(Equal("0"))
			}, 2*time.Minute).Should(Succeed())

			// --- Step 2: Create BatchSandbox with poolRef ---
			By("creating BatchSandbox with poolRef")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"PoolName":         poolName,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pool-pause-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pooled BatchSandbox to reach the steady Succeed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			// --- Step 3: Get pod name from alloc-status ---
			By("getting pod name from alloc-status annotation")
			var podName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty(), "alloc-status annotation should exist")

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocStatus.Pods)).To(BeNumerically(">=", 1))
				podName = allocStatus.Pods[0]
			}).Should(Succeed())
			Expect(podName).NotTo(BeEmpty(), "Should have allocated pod name")

			// --- Step 4: Write marker file ---
			firstMarkerValue := fmt.Sprintf("pool-pause-test-%d", time.Now().UnixNano())
			By("writing marker file into container for rootfs verification")
			cmd = exec.Command("kubectl", "exec", podName, "-n", pauseResumeNamespace,
				"-c", "sandbox-container", "--", "sh", "-c", fmt.Sprintf("printf '%s' > /tmp/pause-marker", firstMarkerValue))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// --- Step 5: Pause - patch spec.pause=true ---
			By("triggering pause by patching spec.pause=true")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox phase to be Paused (template solidified from Pool)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Paused"))
			}, 3*time.Minute).Should(Succeed())

			// Verify template was solidified (no longer nil)
			By("verifying template was solidified from Pool CR")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("sandbox-container"), "Template should be solidified from Pool CR")

			// --- Step 6: Resume - patch spec.pause=false ---
			By("triggering resume by patching spec.pause=false")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for resumed BatchSandbox to return to Succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			// Verify poolRef was cleared after resume
			By("verifying poolRef was cleared after resume")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.spec.poolRef}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "poolRef should be cleared after resume")

			// --- Step 7: Verify rootfs data persistence ---
			By("getting resumed pod name")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace, "-o", "json")
			resumedPodsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodList struct {
				Items []struct {
					Metadata struct {
						Name            string `json:"name"`
						OwnerReferences []struct {
							Kind string `json:"kind"`
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(resumedPodsJSON), &resumedPodList)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodName string
			for _, pod := range resumedPodList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == sandboxName {
						resumedPodName = pod.Metadata.Name
						break
					}
				}
				if resumedPodName != "" {
					break
				}
			}
			Expect(resumedPodName).NotTo(BeEmpty(), "Should find a pod owned by resumed BatchSandbox")

			By("waiting for resumed pod container to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", resumedPodName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.containerStatuses[0].ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("reading marker file from resumed container to verify rootfs persistence")
			cmd = exec.Command("kubectl", "exec", resumedPodName, "-n", pauseResumeNamespace,
				"-c", "sandbox-container", "--", "cat", "/tmp/pause-marker")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(firstMarkerValue), "Rootfs data should persist across pool-based pause/resume")

			// --- Cleanup ---
			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})

		It("should complete pool-based pause-resume with process task scheduling", func() {
			const poolName = "test-pool-pause-task"
			const sandboxName = "test-pool-pause-task-resume"

			By("creating Pool CR with task-executor")
			poolYAML, err := renderTemplate("testdata/pool-with-task-executor.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"Namespace":         pauseResumeNamespace,
				"TaskExecutorImage": utils.TaskExecutorImage,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-pause-task.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to have available pods")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.available}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				g.Expect(output).NotTo(Equal("0"))
			}, 2*time.Minute).Should(Succeed())

			By("creating pooled BatchSandbox with a long-running process task")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-running-task.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"PoolName":         poolName,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pool-pause-task-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the pooled BatchSandbox to allocate and start the task")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.taskRunning}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("getting pooled pod name from alloc-status annotation")
			var pooledPodName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatus.Pods).To(HaveLen(1))
				pooledPodName = allocStatus.Pods[0]
			}).Should(Succeed())
			Expect(pooledPodName).NotTo(BeEmpty())

			taskMarkerValue := fmt.Sprintf("pool-task-pause-test-%d", time.Now().UnixNano())
			By("writing marker file into task-executor container for rootfs verification")
			cmd = exec.Command("kubectl", "exec", pooledPodName, "-n", pauseResumeNamespace,
				"-c", "task-executor", "--", "sh", "-c", fmt.Sprintf("printf '%s' > /tmp/pause-marker", taskMarkerValue))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering pause by patching spec.pause=true")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the BatchSandbox to reach Paused")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Paused"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.allocated}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("0"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.ready}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("0"))
			}, 3*time.Minute).Should(Succeed())

			By("waiting for the internal SandboxSnapshot to succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", sandboxName+"-pause",
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 3*time.Minute).Should(Succeed())

			By("verifying the snapshot records the pushed image digest")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", sandboxName+"-pause",
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.containers[0].imageDigest}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(HavePrefix("sha256:"))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying template was solidified from Pool CR")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("task-executor"))

			By("triggering resume by patching spec.pause=false")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the resumed BatchSandbox to return to Succeed and taskRunning=1")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.taskRunning}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 3*time.Minute).Should(Succeed())

			By("verifying poolRef was cleared after resume")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.spec.poolRef}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("getting resumed pod name owned by the BatchSandbox")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace, "-o", "json")
			resumedPodsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodList struct {
				Items []struct {
					Metadata struct {
						Name            string `json:"name"`
						OwnerReferences []struct {
							Kind string `json:"kind"`
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(resumedPodsJSON), &resumedPodList)
			Expect(err).NotTo(HaveOccurred())

			var resumedPodName string
			for _, pod := range resumedPodList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == sandboxName {
						resumedPodName = pod.Metadata.Name
						break
					}
				}
				if resumedPodName != "" {
					break
				}
			}
			Expect(resumedPodName).NotTo(BeEmpty())

			By("waiting for resumed task-executor container to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", resumedPodName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.containerStatuses[0].ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("reading marker file from resumed task-executor container to verify rootfs persistence")
			cmd = exec.Command("kubectl", "exec", resumedPodName, "-n", pauseResumeNamespace,
				"-c", "task-executor", "--", "cat", "/tmp/pause-marker")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(taskMarkerValue))

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})
	})

	Context("Pause Resume Failure", func() {
		It("should transition snapshot to Failed when source sandbox does not exist", func() {
			const snapshotName = "test-pause-fail-no-source"

			By("creating SandboxSnapshot with non-existent source sandbox")
			snapshotYAML, err := renderTemplate("testdata/sandboxsnapshot.yaml", map[string]interface{}{
				"SnapshotName": snapshotName,
				"Namespace":    pauseResumeNamespace,
				"SandboxName":  "nonexistent-sandbox",
			})
			Expect(err).NotTo(HaveOccurred())

			snapshotFile := filepath.Join("/tmp", "test-pause-fail-snapshot.yaml")
			err = os.WriteFile(snapshotFile, []byte(snapshotYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(snapshotFile)

			cmd := exec.Command("kubectl", "apply", "-f", snapshotFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SandboxSnapshot to reach Failed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Failed"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying Failed condition message contains useful information")
			cmd = exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Failed')].message}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("nonexistent-sandbox"), "Error message should mention the missing sandbox")

			By("verifying no commit job was created")
			cmd = exec.Command("kubectl", "get", "job", "-n", pauseResumeNamespace,
				"-l", "sandbox.opensandbox.io/sandbox-snapshot-name="+snapshotName, "-o", "jsonpath={.items}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("[]"), "No commit job should be created for failed snapshot")

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "sandboxsnapshot", snapshotName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})

		It("should set Phase=Succeed+PauseFailed when commit/push fails with invalid registry", func() {
			const sandboxName = "test-pause-commit-fail"

			By("creating BatchSandbox with template")
			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"SandboxImage":     utils.SandboxImage,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pause-commit-fail-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to reach the steady Succeed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			By("patching registry push secret to invalid value (invalid docker config)")
			// Create an invalid docker config JSON (base64 encoded)
			invalidConfig := `{"auths":{"docker-registry.default.svc.cluster.local:5000":{"username":"invalid","password":"wrong","auth":"aW52YWxpZDp3cm9uZw=="}}}`
			encoded := base64.StdEncoding.EncodeToString([]byte(invalidConfig))
			patchData := fmt.Sprintf(`{"data":{".dockerconfigjson":"%s"}}`, encoded)
			cmd = exec.Command("kubectl", "patch", "secret", "registry-snapshot-push-secret", "-n", pauseResumeNamespace,
				"--type=merge", "-p", patchData)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering pause with invalid registry config")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to remain in Succeed with PauseFailed condition")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 6*time.Minute).Should(Succeed())

			By("verifying PauseFailed condition is set (commit/push failed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='PauseFailed')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 4*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)

			By("restoring registry push secret to valid credentials")
			err = createDockerRegistrySecrets(pauseResumeNamespace)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should set Phase=Failed+ResumeFailed when resume fails with invalid image", func() {
			const sandboxName = "test-resume-image-fail"

			By("creating BatchSandbox with template")
			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"SandboxImage":     utils.SandboxImage,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-resume-image-fail-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to reach the steady Succeed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			By("triggering pause")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Paused")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Paused"))
			}, 3*time.Minute).Should(Succeed())

			By("tampering SandboxSnapshot with invalid image URI")
			cmd = exec.Command("kubectl", "patch", "sandboxsnapshot", sandboxName+"-pause",
				"-n", pauseResumeNamespace, "--type=merge", "--subresource=status",
				"-p", `{"status":{"containers":[{"containerName":"sandbox-container","imageUri":"invalid.registry/unreachable/image:nonexistent"}]}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering resume with tampered snapshot")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Failed with ResumeFailed condition")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// Parse phase from JSON
				var bs struct {
					Status struct {
						Phase string `json:"phase"`
					} `json:"status"`
				}
				json.Unmarshal([]byte(output), &bs)

				if bs.Status.Phase != "Failed" {
					// Print debug info
					cmd = exec.Command("kubectl", "get", "pod", "-n", pauseResumeNamespace,
						"-l", "batch-sandbox.sandbox.opensandbox.io/name="+sandboxName, "-o", "yaml")
					podOutput, _ := utils.Run(cmd)
					fmt.Fprintf(GinkgoWriter, "\n=== Pod status (phase=%s) ===\n%s\n", bs.Status.Phase, podOutput)
				}

				g.Expect(bs.Status.Phase).To(Equal("Failed"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying ResumeFailed condition is set")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='ResumeFailed')].status}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("True"))

			By("verifying the reserved internal SandboxSnapshot is retained after failed resume")
			cmd = exec.Command("kubectl", "get", "sandboxsnapshot", sandboxName+"-pause",
				"-n", pauseResumeNamespace, "-o", "name")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Internal pause snapshot should remain after failed resume")
			Expect(strings.TrimSpace(output)).To(Equal("sandboxsnapshot.sandbox.opensandbox.io/" + sandboxName + "-pause"))

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})

		It("should successfully retry pause after fixing registry config", func() {
			const sandboxName = "test-pause-retry-success"

			By("creating BatchSandbox with template")
			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": sandboxName,
				"Namespace":        pauseResumeNamespace,
				"SandboxImage":     utils.SandboxImage,
				"Replicas":         1,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pause-retry-success-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to reach the steady Succeed phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeed"))
			}, 2*time.Minute).Should(Succeed())

			By("patching registry push secret to invalid value (invalid docker config)")
			invalidConfig := `{"auths":{"docker-registry.default.svc.cluster.local:5000":{"username":"invalid","password":"wrong","auth":"aW52YWxpZDp3cm9uZw=="}}}`
			encoded := base64.StdEncoding.EncodeToString([]byte(invalidConfig))
			patchData := fmt.Sprintf(`{"data":{".dockerconfigjson":"%s"}}`, encoded)
			cmd = exec.Command("kubectl", "patch", "secret", "registry-snapshot-push-secret", "-n", pauseResumeNamespace,
				"--type=merge", "-p", patchData)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering first pause attempt (will fail due to invalid registry)")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pause to fail with PauseFailed condition")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='PauseFailed')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 3*time.Minute).Should(Succeed())

			By("verifying Phase is still Succeed (retryable)")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("Succeed"))

			By("restoring registry push secret to valid credentials")
			err = createDockerRegistrySecrets(pauseResumeNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("resetting spec.pause so the next pause request gets a new generation")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":null}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering retry pause (nil -> true) so PauseFailed can clear and pause can succeed")
			cmd = exec.Command("kubectl", "patch", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "--type=merge",
				"-p", `{"spec":{"pause":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Paused")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Paused"))
			}, 3*time.Minute).Should(Succeed())

			By("verifying PauseFailed condition is cleared")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='PauseFailed')].status}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty(), "PauseFailed condition should be cleared after successful pause")

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace, "--ignore-not-found=true")
			utils.Run(cmd)
		})
	})
})

// createHtpasswdSecret creates the htpasswd secret for registry authentication.
// Docker Registry v2 only supports bcrypt hashes, not MD5 ($apr1$) or SHA1.
func createHtpasswdSecret(namespace string) error {
	htpasswdEntry := ""
	pyCmd := exec.Command("python3", "-c",
		fmt.Sprintf("import bcrypt; print('%s:' + bcrypt.hashpw(b'%s', bcrypt.gensalt(rounds=10)).decode())",
			registryUsername, registryPassword))
	if output, err := pyCmd.Output(); err == nil {
		htpasswdEntry = strings.TrimSpace(string(output))
	}

	if htpasswdEntry == "" {
		return fmt.Errorf("failed to generate bcrypt htpasswd: python3 bcrypt not available")
	}

	tmpFile := filepath.Join(os.TempDir(), "htpasswd")
	if err := os.WriteFile(tmpFile, []byte(htpasswdEntry), 0644); err != nil {
		return fmt.Errorf("failed to write htpasswd file: %w", err)
	}
	defer os.Remove(tmpFile)

	cmd := exec.Command("kubectl", "create", "secret", "generic", "registry-auth",
		"--from-file=htpasswd="+tmpFile, "-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		cmd = exec.Command("kubectl", "delete", "secret", "registry-auth", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "generic", "registry-auth",
			"--from-file=htpasswd="+tmpFile, "-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-auth secret: %w", err)
		}
	}

	return nil
}

// createDockerRegistrySecrets creates docker-registry secrets for push/pull.
func createDockerRegistrySecrets(namespace string) error {
	server := registryServiceAddr

	cmd := exec.Command("kubectl", "create", "secret", "docker-registry", "registry-snapshot-push-secret",
		"--docker-server="+server,
		"--docker-username="+registryUsername,
		"--docker-password="+registryPassword,
		"-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		cmd = exec.Command("kubectl", "delete", "secret", "registry-snapshot-push-secret", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-snapshot-push-secret",
			"--docker-server="+server,
			"--docker-username="+registryUsername,
			"--docker-password="+registryPassword,
			"-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-snapshot-push-secret: %w", err)
		}
	}

	cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-pull-secret",
		"--docker-server="+server,
		"--docker-username="+registryUsername,
		"--docker-password="+registryPassword,
		"-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		cmd = exec.Command("kubectl", "delete", "secret", "registry-pull-secret", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-pull-secret",
			"--docker-server="+server,
			"--docker-username="+registryUsername,
			"--docker-password="+registryPassword,
			"-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-pull-secret: %w", err)
		}
	}

	return nil
}
