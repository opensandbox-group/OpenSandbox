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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

// namespace where the project is deployed in
const namespace = "opensandbox-system"

var _ = Describe("Manager", Ordered, Label("Core"), func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		if err != nil {
			// Ignore error if namespace already exists
			Expect(err.Error()).To(ContainSubstring("AlreadyExists"), "Failed to create namespace")
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
		cmd = exec.Command("make", "deploy", fmt.Sprintf("CONTROLLER_IMG=%s", utils.ControllerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", Label("Manager"), func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				goTemplate := `{{ range .items }}` +
					`{{ if not .metadata.deletionTimestamp }}` +
					`{{ .metadata.name }}` +
					`{{ "\n" }}{{ end }}{{ end }}`
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template="+goTemplate,
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})
	})

	Context("Pool", Label("Pool"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should handle pod eviction correctly", func() {
			const poolName = "test-pool-eviction"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    4,
				"BufferMin":    2,
				"PoolMax":      6,
				"PoolMin":      3,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool pods to be Running")
			var allPoolPods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allPoolPods = strings.Fields(output)
				g.Expect(len(allPoolPods)).To(BeNumerically(">=", 3))
			}, 3*time.Minute).Should(Succeed())

			By("allocating pods via BatchSandbox")
			const batchSandboxName = "test-bs-eviction"
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var allocatedPods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())

				var alloc struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(out), &alloc)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedPods = alloc.Pods
				g.Expect(allocatedPods).To(HaveLen(2))
			}, 2*time.Minute).Should(Succeed())

			By("marking all pool pods for eviction")
			for _, pod := range allPoolPods {
				cmd := exec.Command("kubectl", "label", "pod", pod, "-n", testNamespace,
					"pool.opensandbox.io/evict=true", "--overwrite")
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("verifying allocated pods are not deleted")
			Consistently(func(g Gomega) {
				for _, pod := range allocatedPods {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "allocated pod %s should still exist", pod)
					g.Expect(output).To(BeEmpty(), "allocated pod %s should not be terminating", pod)
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should evict idle pool pods", func() {
			const poolName = "test-pool-eviction-b"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      3,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool to stabilise with idle pods")
			var idlePods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				idlePods = strings.Fields(output)
				g.Expect(len(idlePods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("marking all pool pods for eviction")
			for _, pod := range idlePods {
				cmd := exec.Command("kubectl", "label", "pod", pod, "-n", testNamespace,
					"pool.opensandbox.io/evict=true", "--overwrite")
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("verifying all idle pods are eventually deleted")
			Eventually(func(g Gomega) {
				for _, pod := range idlePods {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					if err != nil && strings.Contains(err.Error(), "not found") {
						continue // already gone
					}
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).NotTo(BeEmpty(), "idle pod %s should be terminating", pod)
				}
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should not allocate pods marked for eviction to a BatchSandbox", func() {
			const poolName = "test-pool-eviction-c"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    4,
				"BufferMin":    2,
				"PoolMax":      6,
				"PoolMin":      3,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to replenish with fresh idle pods")
			var freshPods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				freshPods = strings.Fields(output)
				g.Expect(len(freshPods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("marking all current idle pods for eviction before any BatchSandbox claims them")
			for _, pod := range freshPods {
				cmd := exec.Command("kubectl", "label", "pod", pod, "-n", testNamespace,
					"pool.opensandbox.io/evict=true", "--overwrite")
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}
			evictingPods := make(map[string]bool)
			for _, pod := range freshPods {
				evictingPods[pod] = true
			}

			By("creating a BatchSandbox that requests pods from the pool")
			const batchSandboxName = "test-bs-eviction-c"
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying BatchSandbox gets a pod that is NOT one of the evicting pods")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())

				var alloc struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(out), &alloc)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(alloc.Pods).To(HaveLen(1))
				g.Expect(evictingPods).NotTo(HaveKey(alloc.Pods[0]),
					"evicting pod %s should not be allocated", alloc.Pods[0])
			}, 3*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should correctly create pods and maintain pool status", func() {
			const poolName = "test-pool-basic"
			const testNamespace = "default"
			const poolMin = 2
			const poolMax = 5
			const bufferMin = 1
			const bufferMax = 3

			By("creating a basic Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    bufferMax,
				"BufferMin":    bufferMin,
				"PoolMax":      poolMax,
				"PoolMin":      poolMin,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-basic.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Pool")

			By("verifying Pool creates pods and maintains correct status")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status}")
				statusOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(statusOutput).To(ContainSubstring(`"total":`), "Pool status should have total field")
				g.Expect(statusOutput).To(ContainSubstring(`"allocated":`), "Pool status should have allocated field")
				g.Expect(statusOutput).To(ContainSubstring(`"available":`), "Pool status should have available field")

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				if totalStr != "" {
					fmt.Sscanf(totalStr, "%d", &total)
				}
				g.Expect(total).To(BeNumerically(">=", poolMin), "Pool total should be >= poolMin")
				g.Expect(total).To(BeNumerically("<=", poolMax), "Pool total should be <= poolMax")
			}, 2*time.Minute).Should(Succeed())

			By("verifying pods are created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Pool should create pods")
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up the Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should correctly manage capacity when poolMin and poolMax change", func() {
			const poolName = "test-pool-capacity"
			const testNamespace = "default"

			By("creating a Pool with initial capacity")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    1,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-capacity.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for initial Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				if totalStr != "" {
					fmt.Sscanf(totalStr, "%d", &total)
				}
				g.Expect(total).To(BeNumerically(">=", 2))
			}, 2*time.Minute).Should(Succeed())

			By("increasing poolMin to trigger scale up")
			poolYAML, err = renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    1,
				"PoolMax":      10,
				"PoolMin":      5,
			})
			Expect(err).NotTo(HaveOccurred())

			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool scales up to meet new poolMin")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				if totalStr != "" {
					fmt.Sscanf(totalStr, "%d", &total)
				}
				g.Expect(total).To(BeNumerically(">=", 5), "Pool should scale up to meet poolMin=5")
				g.Expect(total).To(BeNumerically("<=", 10), "Pool should not exceed poolMax=10")
			}, 2*time.Minute).Should(Succeed())

			By("decreasing poolMax to below current total")
			poolYAML, err = renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      3,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool respects new poolMax constraint")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				if totalStr != "" {
					fmt.Sscanf(totalStr, "%d", &total)
				}
				g.Expect(total).To(BeNumerically("<=", 3), "Pool should scale down to meet poolMax=3")
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up the Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should upgrade pool template correctly", func() {
			const poolName = "test-pool-upgrade"
			const testNamespace = "default"
			const batchSandboxName = "test-bs-for-upgrade"

			By("creating a Pool with initial template")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-upgrade.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("allocating a pod from the pool via BatchSandbox")
			batchSandboxYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-upgrade.yaml")
			err = os.WriteFile(bsFile, []byte(batchSandboxYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to allocate pod")
			var allocatedPodNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty(), "alloc-status annotation should exist")

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())

				allocatedPodNames = allocStatus.Pods
				g.Expect(len(allocatedPodNames)).To(Equal(1), "Should have 1 allocated pod")
			}, 2*time.Minute).Should(Succeed())

			By("getting all pool pods")
			cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
				"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
				"-o", "jsonpath={.items[*].metadata.name}")
			allPoolPodsStr, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			allPoolPods := strings.Fields(allPoolPodsStr)

			By("calculating available pods (all pool pods - allocated pods)")
			availablePodsBeforeUpgrade := []string{}
			allocatedPodMap := make(map[string]bool)
			for _, podName := range allocatedPodNames {
				allocatedPodMap[podName] = true
			}
			for _, podName := range allPoolPods {
				if !allocatedPodMap[podName] {
					availablePodsBeforeUpgrade = append(availablePodsBeforeUpgrade, podName)
				}
			}

			By("updating Pool template with new environment variable")
			updatedPoolYAML, err := renderTemplate("testdata/pool-with-env.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"Namespace":    testNamespace,
				"SandboxImage": utils.SandboxImage,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			err = os.WriteFile(poolFile, []byte(updatedPoolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying allocated pod is NOT upgraded")
			Consistently(func(g Gomega) {
				for _, allocatedPod := range allocatedPodNames {
					cmd := exec.Command("kubectl", "get", "pod", allocatedPod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal(allocatedPod), "Allocated pod should not be recreated")
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying available pods are recreated with new template")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].metadata.name}")
				allPodsAfterStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allPodsAfter := strings.Fields(allPodsAfterStr)

				// Get currently allocated pods
				cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())

				currentAllocatedPods := make(map[string]bool)
				for _, podName := range allocStatus.Pods {
					currentAllocatedPods[podName] = true
				}

				// Calculate available pods after upgrade
				availablePodsAfterUpgrade := []string{}
				for _, podName := range allPodsAfter {
					if !currentAllocatedPods[podName] {
						availablePodsAfterUpgrade = append(availablePodsAfterUpgrade, podName)
					}
				}

				// Check if at least one available pod was recreated
				recreated := false
				for _, oldPod := range availablePodsBeforeUpgrade {
					found := false
					for _, newPod := range availablePodsAfterUpgrade {
						if oldPod == newPod {
							found = true
							break
						}
					}
					if !found {
						recreated = true
						break
					}
				}
				g.Expect(recreated).To(BeTrue(), "At least one available pod should be recreated")
			}, 3*time.Minute).Should(Succeed())

			By("verifying new pods have the upgraded environment variable")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Spec struct {
							Containers []struct {
								Name string `json:"name"`
								Env  []struct {
									Name  string `json:"name"`
									Value string `json:"value"`
								} `json:"env"`
							} `json:"containers"`
						} `json:"spec"`
					} `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &podList)
				g.Expect(err).NotTo(HaveOccurred())

				// Get currently allocated pods
				cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())

				allocatedPodMap := make(map[string]bool)
				for _, podName := range allocStatus.Pods {
					allocatedPodMap[podName] = true
				}

				// Find at least one available pod with UPGRADED=true
				foundUpgraded := false
				for _, pod := range podList.Items {
					if !allocatedPodMap[pod.Metadata.Name] {
						// This is an available pod
						for _, container := range pod.Spec.Containers {
							if container.Name == "sandbox-container" {
								for _, env := range container.Env {
									if env.Name == "UPGRADED" && env.Value == "true" {
										foundUpgraded = true
										break
									}
								}
							}
						}
					}
				}
				g.Expect(foundUpgraded).To(BeTrue(), "At least one available pod should have UPGRADED=true env var")
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up BatchSandbox and Pool")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("BatchSandbox", Label("Batch"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should work correctly in non-pooled mode", func() {
			const batchSandboxName = "test-bs-non-pooled"
			const testNamespace = "default"
			const replicas = 2

			By("creating a non-pooled BatchSandbox")
			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-non-pooled.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods are created directly from template")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Metadata struct {
							Name            string `json:"name"`
							OwnerReferences []struct {
								Kind string `json:"kind"`
								Name string `json:"name"`
								UID  string `json:"uid"`
							} `json:"ownerReferences"`
						} `json:"metadata"`
					} `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &podList)
				g.Expect(err).NotTo(HaveOccurred())

				// Find pods owned by this BatchSandbox
				ownedPods := []string{}
				for _, pod := range podList.Items {
					for _, owner := range pod.Metadata.OwnerReferences {
						if owner.Kind == "BatchSandbox" && owner.Name == batchSandboxName {
							ownedPods = append(ownedPods, pod.Metadata.Name)
							break
						}
					}
				}
				g.Expect(len(ownedPods)).To(Equal(replicas), "Should create %d pods", replicas)
			}, 2*time.Minute).Should(Succeed())

			By("verifying BatchSandbox status is correctly updated")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status}")
				statusOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"replicas":%d`, replicas)))
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"allocated":%d`, replicas)))
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"ready":%d`, replicas)))
			}, 2*time.Minute).Should(Succeed())

			By("verifying endpoint annotation is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/endpoints}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				endpoints := strings.Split(output, ",")
				g.Expect(len(endpoints)).To(Equal(replicas))
			}, 30*time.Second).Should(Succeed())

			By("cleaning up BatchSandbox")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods are deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Metadata struct {
							Name              string  `json:"name"`
							DeletionTimestamp *string `json:"deletionTimestamp"`
							OwnerReferences   []struct {
								Kind string `json:"kind"`
								Name string `json:"name"`
							} `json:"ownerReferences"`
						} `json:"metadata"`
					} `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &podList)
				g.Expect(err).NotTo(HaveOccurred())

				// Check no pods are owned by this BatchSandbox or they have deletionTimestamp
				for _, pod := range podList.Items {
					for _, owner := range pod.Metadata.OwnerReferences {
						if owner.Kind == "BatchSandbox" && owner.Name == batchSandboxName {
							g.Expect(pod.Metadata.DeletionTimestamp).NotTo(BeNil(),
								"Pod %s owned by BatchSandbox should have deletionTimestamp set", pod.Metadata.Name)
						}
					}
				}
			}, 2*time.Minute).Should(Succeed())
		})

		It("should work correctly in pooled mode", func() {
			const poolName = "test-pool-for-bs"
			const batchSandboxName = "test-bs-pooled"
			const testNamespace = "default"
			const replicas = 2

			By("creating a Pool")
			// Use a zero-buffer pool so restart recovery verifies allocation reconstruction
			// without racing the normal buffer refill behavior.
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    0,
				"BufferMin":    0,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-for-bs.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating a pooled BatchSandbox")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-pooled.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying BatchSandbox allocates pods from pool")
			Eventually(func(g Gomega) {
				// Verify alloc-status annotation contains pool pod names
				cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty(), "alloc-status annotation should exist")

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocStatus.Pods)).To(Equal(replicas), "Should have %d pods in alloc-status", replicas)

				// Verify the pods in alloc-status are from the pool
				for _, podName := range allocStatus.Pods {
					cmd = exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.labels.sandbox\\.opensandbox\\.io/pool-name}")
					poolLabel, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(poolLabel).To(Equal(poolName), "Pod %s should be from pool %s", podName, poolName)
				}
			}, 2*time.Minute).Should(Succeed())

			By("verifying BatchSandbox status is correctly updated")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status}")
				statusOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"replicas":%d`, replicas)))
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"ready":%d`, replicas)))
			}, 30*time.Second).Should(Succeed())

			By("verifying endpoint annotation is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/endpoints}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				endpoints := strings.Split(output, ",")
				g.Expect(len(endpoints)).To(Equal(replicas))
			}, 30*time.Second).Should(Succeed())

			By("recording Pool allocated count")
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			allocatedBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("cleaning up BatchSandbox")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods are returned to pool")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedAfter, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				before := 0
				if allocatedBefore != "" {
					fmt.Sscanf(allocatedBefore, "%d", &before)
				}
				after := 0
				if allocatedAfter != "" {
					fmt.Sscanf(allocatedAfter, "%d", &after)
				}
				g.Expect(after).To(BeNumerically("<", before), "Allocated count should decrease")
			}, 30*time.Second).Should(Succeed())

			By("cleaning up Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should expire and delete non-pooled BatchSandbox correctly", func() {
			const batchSandboxName = "test-bs-expire-non-pooled"
			const testNamespace = "default"
			const replicas = 1

			By("creating a non-pooled BatchSandbox with expireTime")
			expireTime := time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339)

			bsYAML, err := renderTemplate("testdata/batchsandbox-non-pooled-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"ExpireTime":       expireTime,
				"SandboxImage":     utils.SandboxImage,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-expire-non-pooled.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying BatchSandbox is created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)))
			}, 2*time.Minute).Should(Succeed())

			By("recording pod names")
			cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-o", "json")
			output, err := utils.Run(cmd)
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
			err = json.Unmarshal([]byte(output), &podList)
			Expect(err).NotTo(HaveOccurred())

			podNamesList := []string{}
			for _, pod := range podList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == batchSandboxName {
						podNamesList = append(podNamesList, pod.Metadata.Name)
						break
					}
				}
			}
			Expect(len(podNamesList)).To(BeNumerically(">", 0), "Should have pods owned by BatchSandbox")

			By("waiting for BatchSandbox to expire and be deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying pods are deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var currentPodList struct {
					Items []struct {
						Metadata struct {
							Name              string  `json:"name"`
							DeletionTimestamp *string `json:"deletionTimestamp"`
							OwnerReferences   []struct {
								Kind string `json:"kind"`
								Name string `json:"name"`
							} `json:"ownerReferences"`
						} `json:"metadata"`
					} `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &currentPodList)
				g.Expect(err).NotTo(HaveOccurred())

				// Verify no pods are owned by the deleted BatchSandbox or they have deletionTimestamp
				for _, pod := range currentPodList.Items {
					for _, owner := range pod.Metadata.OwnerReferences {
						if owner.Kind == "BatchSandbox" && owner.Name == batchSandboxName {
							g.Expect(pod.Metadata.DeletionTimestamp).NotTo(BeNil(),
								"Pod %s owned by BatchSandbox should have deletionTimestamp set", pod.Metadata.Name)
						}
					}
				}
			}, 30*time.Second).Should(Succeed())
		})

		It("should expire and return pooled BatchSandbox pods to pool", func() {
			const poolName = "test-pool-for-expire"
			const batchSandboxName = "test-bs-expire-pooled"
			const testNamespace = "default"
			const replicas = 1

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-for-expire.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("recording Pool allocated count before BatchSandbox creation")
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			allocatedBeforeBS, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a pooled BatchSandbox with expireTime")
			expireTime := time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339)
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       expireTime,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-expire-pooled.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("recording pod names from alloc-status")
			var podNamesList []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				podNamesList = allocStatus.Pods
				g.Expect(len(podNamesList)).To(BeNumerically(">", 0), "Should have allocated pods")
			}, 2*time.Minute).Should(Succeed())

			allocatedAfterBS := ""
			By("verifying Pool allocated count increased after BatchSandbox allocation")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				_allocatedAfterBS, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedAfterBS = _allocatedAfterBS

				before := 0
				if allocatedBeforeBS != "" {
					fmt.Sscanf(allocatedBeforeBS, "%d", &before)
				}

				after := 0
				if _allocatedAfterBS != "" {
					fmt.Sscanf(allocatedAfterBS, "%d", &after)
				}

				g.Expect(after).To(BeNumerically(">", before), "Pool allocated count should increase after BatchSandbox allocation")
			}, 30*time.Second).Should(Succeed())

			By("waiting for BatchSandbox to expire and be deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying pods still exist and are returned to pool")
			Eventually(func(g Gomega) {
				for _, podName := range podNamesList {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal(podName), "Pod should still exist")
				}
			}, 30*time.Second).Should(Succeed())

			By("verifying Pool allocated count decreased after BatchSandbox expiration")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedAfterExpiration, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				before := 0
				if allocatedAfterBS != "" {
					fmt.Sscanf(allocatedAfterBS, "%d", &before)
				}
				after := 0
				if allocatedAfterExpiration != "" {
					fmt.Sscanf(allocatedAfterExpiration, "%d", &after)
				}
				g.Expect(after).To(BeNumerically("<", before), "Allocated count should decrease")
			}, 30*time.Second).Should(Succeed())

			By("cleaning up Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Task", Label("Task"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should successfully manage Pool with task scheduling", func() {
			const poolName = "test-pool"
			const batchSandboxName = "test-batchsandbox-with-task"
			const testNamespace = "default"
			const replicas = 2

			By("creating a Pool with task-executor sidecar")
			poolTemplateFile := filepath.Join("testdata", "pool-with-task-executor.yaml")
			poolYAML, err := renderTemplate(poolTemplateFile, map[string]interface{}{
				"PoolName":          poolName,
				"Namespace":         testNamespace,
				"TaskExecutorImage": utils.TaskExecutorImage,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Pool")

			By("waiting for Pool to be ready")
			verifyPoolReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				By(fmt.Sprintf("waiting for Pool to be ready, output %s", output))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "Pool status.total should not be empty")
			}
			Eventually(verifyPoolReady, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with process-based tasks using the Pool")
			batchSandboxTemplateFile := filepath.Join("testdata", "batchsandbox-with-process-task.yaml")
			batchSandboxYAML, err := renderTemplate(batchSandboxTemplateFile, map[string]interface{}{
				"BatchSandboxName":  batchSandboxName,
				"Namespace":         testNamespace,
				"Replicas":          replicas,
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
			})
			Expect(err).NotTo(HaveOccurred())

			batchSandboxFile := filepath.Join("/tmp", "test-batchsandbox.yaml")
			err = os.WriteFile(batchSandboxFile, []byte(batchSandboxYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", batchSandboxFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create BatchSandbox")

			By("verifying BatchSandbox successfully allocated endpoints")
			verifyBatchSandboxAllocated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)), "BatchSandbox should allocate %d replicas", replicas)
			}
			Eventually(verifyBatchSandboxAllocated, 2*time.Minute).Should(Succeed())

			By("verifying BatchSandbox endpoints are available")
			verifyEndpoints := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/endpoints}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(), "BatchSandbox should have sandbox.opensandbox.io/endpoints annotation")
				endpoints := strings.Split(output, ",")
				g.Expect(len(endpoints)).To(Equal(replicas), "Should have %d endpoints", replicas)
			}
			Eventually(verifyEndpoints, 30*time.Second).Should(Succeed())

			By("verifying BatchSandbox status is as expected")
			verifyBatchSandboxStatus := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status}")
				statusOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"replicas":%d`, replicas)))
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"allocated":%d`, replicas)))
				g.Expect(statusOutput).To(ContainSubstring(fmt.Sprintf(`"ready":%d`, replicas)))
			}
			Eventually(verifyBatchSandboxStatus, 30*time.Second).Should(Succeed())

			By("verifying all tasks are successfully scheduled and succeeded")
			verifyTasksSucceeded := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.taskSucceed}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)), "All tasks should succeed")

				cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.taskFailed}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("0"), "No tasks should fail")
			}
			Eventually(verifyTasksSucceeded, 2*time.Minute).Should(Succeed())

			By("recording Pool status before deletion")
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			poolAllocatedBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("deleting the BatchSandbox")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete BatchSandbox")

			By("verifying all tasks are unloaded and BatchSandbox is deleted")
			verifyBatchSandboxDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "BatchSandbox should be deleted")
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}
			Eventually(verifyBatchSandboxDeleted, 2*time.Minute).Should(Succeed())

			By("verifying pods are returned to the Pool")
			verifyPodsReturnedToPool := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				poolAllocatedAfter, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				beforeCount := 0
				if poolAllocatedBefore != "" {
					fmt.Sscanf(poolAllocatedBefore, "%d", &beforeCount)
				}
				afterCount := 0
				if poolAllocatedAfter != "" {
					fmt.Sscanf(poolAllocatedAfter, "%d", &afterCount)
				}
				g.Expect(afterCount).To(BeNumerically("<=", beforeCount),
					"Pool allocated count should decrease or stay same after BatchSandbox deletion")
			}
			Eventually(verifyPodsReturnedToPool, 30*time.Second).Should(Succeed())

			By("cleaning up the Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete Pool")

			By("cleaning up temporary files")
			os.Remove(poolFile)
			os.Remove(batchSandboxFile)
		})
	})

	Context("Pool Update", Label("Pool"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should perform rolling update with maxUnavailable constraint", func() {
			const poolName = "test-pool-rolling-update"
			const testNamespace = "default"
			const poolSize = 10
			const maxUnavailablePercent = "20%"

			By("creating a Pool with updateStrategy")
			poolYAML, err := renderTemplate("testdata/pool-with-update-strategy.yaml", map[string]interface{}{
				"PoolName":       poolName,
				"SandboxImage":   utils.SandboxImage,
				"Namespace":      testNamespace,
				"BufferMax":      poolSize,
				"BufferMin":      poolSize - 2,
				"PoolMax":        poolSize,
				"PoolMin":        poolSize,
				"MaxUnavailable": maxUnavailablePercent,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to have all pods running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(totalStr, "%d", &total)
				g.Expect(total).To(Equal(poolSize))

				cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(strings.Fields(output))).To(Equal(poolSize))
			}, 3*time.Minute).Should(Succeed())

			By("allocating some pods via BatchSandbox")
			const batchSandboxName = "test-bs-rolling-update"
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var allocatedPods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())

				var alloc struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(out), &alloc)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedPods = alloc.Pods
				g.Expect(allocatedPods).To(HaveLen(3))
			}, 2*time.Minute).Should(Succeed())

			By("recording initial revision from pool status")
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.revision}")
			initialRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("triggering pool update by changing template")
			updatedPoolYAML, err := renderTemplate("testdata/pool-with-update-strategy.yaml", map[string]interface{}{
				"PoolName":       poolName,
				"SandboxImage":   utils.SandboxImage,
				"Namespace":      testNamespace,
				"BufferMax":      poolSize,
				"BufferMin":      poolSize - 2,
				"PoolMax":        poolSize,
				"PoolMin":        poolSize,
				"MaxUnavailable": maxUnavailablePercent,
				"EnvValue":       "v2",
			})
			Expect(err).NotTo(HaveOccurred())

			updatedPoolWithEnv := strings.Replace(updatedPoolYAML, "command: [\"sleep\", \"3600\"]",
				"command: [\"sleep\", \"3600\"]\n        env:\n        - name: VERSION\n          value: \"v2\"", 1)
			err = os.WriteFile(poolFile, []byte(updatedPoolWithEnv), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying allocated pods are not deleted during upgrade")
			Consistently(func(g Gomega) {
				for _, pod := range allocatedPods {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "allocated pod %s should still exist", pod)
					g.Expect(output).To(BeEmpty(), "allocated pod %s should not be terminating", pod)
				}
			}, 60*time.Second, 5*time.Second).Should(Succeed())

			By("verifying new BatchSandbox can be allocated during upgrade")
			const newBatchSandboxName = "test-bs-rolling-update-new"
			newBSYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": newBatchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			newBSFile := filepath.Join("/tmp", newBatchSandboxName+".yaml")
			err = os.WriteFile(newBSFile, []byte(newBSYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(newBSFile)

			cmd = exec.Command("kubectl", "apply", "-f", newBSFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying maxUnavailable constraint during upgrade")
			maxUnavailable := int(float64(poolSize) * 0.2)
			if maxUnavailable < 1 {
				maxUnavailable = 1
			}

			// Check a few times that unavailable pods don't exceed maxUnavailable
			for i := 0; i < 5; i++ {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "json")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Status struct {
							Phase      string `json:"phase"`
							Conditions []struct {
								Type   string `json:"type"`
								Status string `json:"status"`
							} `json:"conditions"`
						} `json:"status"`
					} `json:"items"`
				}
				err = json.Unmarshal([]byte(output), &podList)
				Expect(err).NotTo(HaveOccurred())

				unavailableCount := 0
				for _, pod := range podList.Items {
					if pod.Status.Phase != "Running" {
						unavailableCount++
						continue
					}
					ready := false
					for _, cond := range pod.Status.Conditions {
						if cond.Type == "Ready" && cond.Status == "True" {
							ready = true
							break
						}
					}
					if !ready {
						unavailableCount++
					}
				}
				Expect(unavailableCount).To(BeNumerically("<=", maxUnavailable+1),
					"unavailable pods should not exceed maxUnavailable + 1 (allowing for timing)")
				time.Sleep(2 * time.Second)
			}

			By("verifying pool status reflects upgrade progress")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.updated}")
				updatedStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				updated := 0
				if updatedStr != "" {
					fmt.Sscanf(updatedStr, "%d", &updated)
				}
				// At least some pods should be updated
				g.Expect(updated).To(BeNumerically(">", 0), "some pods should be updated")

				// Revision should be different from initial
				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.revision}")
				currentRevision, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(currentRevision).NotTo(Equal(initialRevision))
			}, 2*time.Minute).Should(Succeed())

			By("releasing BatchSandbox to allow full upgrade")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "batchsandbox", newBatchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("verifying pool eventually completes upgrade")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.updated}")
				updatedStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				updated := 0
				if updatedStr != "" {
					fmt.Sscanf(updatedStr, "%d", &updated)
				}
				g.Expect(updated).To(Equal(poolSize), "all pods should be updated")
			}, 3*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Pool State Recovery", Label("Pool"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should reconstruct pool allocation state after controller restart", func() {
			const poolName = "test-pool-recovery"
			const batchSandboxName = "test-bs-recovery"
			const testNamespace = "default"
			const replicas = 2

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-recovery.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox that allocates from the pool")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-recovery.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to allocate pods")
			var poolAllocatedBefore string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)))

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				poolAllocatedBefore, _ = utils.Run(cmd)
			}, 2*time.Minute).Should(Succeed())

			By("wait Pool to be stable")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(totalStr, "%d", &total)

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				availableStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				available := 0
				fmt.Sscanf(availableStr, "%d", &available)

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)

				g.Expect(available+allocated).To(Equal(total), "Pool available+allocated should equal total")
				// Ensure buffer (available) is within [BufferMin, BufferMax] so the pool is truly stable
				// before restarting, preventing spurious scale-up after restart.
				g.Expect(available).To(BeNumerically(">=", 2), "Pool available should be >= BufferMin")
				g.Expect(available).To(BeNumerically("<=", 3), "Pool available should be <= BufferMax")
			}, 2*time.Minute).Should(Succeed())

			By("recording Pool available count before restart")
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.available}")
			poolAvailableBefore, _ := utils.Run(cmd)

			By("restarting the controller")
			err = restartController()
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool allocation state is reconstructed after restart")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				poolAllocatedAfter, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(poolAllocatedAfter).To(Equal(poolAllocatedBefore))

				// Wait for pool to stabilize: available+allocated must equal total before checking available.
				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(totalStr, "%d", &total)

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				poolAvailableAfter, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				available := 0
				fmt.Sscanf(poolAvailableAfter, "%d", &available)
				allocated := 0
				fmt.Sscanf(poolAllocatedAfter, "%d", &allocated)
				g.Expect(available+allocated).To(Equal(total), "Pool must stabilize before checking available")

				g.Expect(poolAvailableAfter).To(Equal(poolAvailableBefore))
			}, 30*time.Second).Should(Succeed())

			By("creating new BatchSandbox to verify no duplicate allocation occurs")
			const newBatchSandboxName = "test-bs-recovery-new"
			newBSYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": newBatchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			newBSFile := filepath.Join("/tmp", "test-bs-recovery-new.yaml")
			err = os.WriteFile(newBSFile, []byte(newBSYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(newBSFile)

			cmd = exec.Command("kubectl", "apply", "-f", newBSFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying new BatchSandbox gets allocated and Pool.allocated increases correctly")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				poolAllocatedNew, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				before := 0
				if poolAllocatedBefore != "" {
					fmt.Sscanf(poolAllocatedBefore, "%d", &before)
				}
				after := 0
				if poolAllocatedNew != "" {
					fmt.Sscanf(poolAllocatedNew, "%d", &after)
				}
				g.Expect(after).To(Equal(before+1), "Pool.allocated should increase by 1")
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up resources")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "batchsandbox", newBatchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should reconstruct allocation for multiple batchsandboxes after restart", func() {
			const poolName = "test-pool-multi-bs"
			const bs1Name = "test-bs-1"
			const bs2Name = "test-bs-2"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    5,
				"BufferMin":    3,
				"PoolMax":      10,
				"PoolMin":      5,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-multi.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating two BatchSandboxes")
			bs1YAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bs1Name,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())
			bs1File := filepath.Join("/tmp", "test-bs1.yaml")
			err = os.WriteFile(bs1File, []byte(bs1YAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bs1File)

			bs2YAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bs2Name,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())
			bs2File := filepath.Join("/tmp", "test-bs2.yaml")
			err = os.WriteFile(bs2File, []byte(bs2YAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bs2File)

			cmd = exec.Command("kubectl", "apply", "-f", bs1File)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "apply", "-f", bs2File)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for allocations to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bs1Name, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output1, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output1).To(Equal("2"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", bs2Name, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output2, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output2).To(Equal("3"))

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(5))
			}, 2*time.Minute).Should(Succeed())

			By("restarting the controller")
			err = restartController()
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool allocation state is correctly reconstructed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(5))

				cmd = exec.Command("kubectl", "get", "batchsandbox", bs1Name, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output1, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output1).To(Equal("2"))

				cmd = exec.Command("kubectl", "get", "batchsandbox", bs2Name, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output2, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output2).To(Equal("3"))
			}, 30*time.Second).Should(Succeed())

			By("deleting first BatchSandbox and verifying only its pods are returned")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bs1Name, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(3))
			}, 30*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bs2Name, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should handle released pods correctly after controller restart", func() {
			const poolName = "test-pool-release"
			const batchSandboxName = "test-bs-release"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    5,
				"BufferMin":    3,
				"PoolMax":      10,
				"PoolMin":      5,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-release.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with replicas=3")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-release.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for allocation")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(3))
			}, 2*time.Minute).Should(Succeed())

			By("deleting BatchSandbox to release pods")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pods to be released")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(0))
			}, 30*time.Second).Should(Succeed())

			By("restarting the controller after pods are released")
			err = restartController()
			Expect(err).NotTo(HaveOccurred())

			By("verifying released pods are not re-allocated")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(0))
			}, 10*time.Second, 2*time.Second).Should(Succeed())

			By("creating new BatchSandbox to verify it gets fresh pods")
			const newBatchSandboxName = "test-bs-release-new"
			newBSYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": newBatchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			newBSFile := filepath.Join("/tmp", "test-bs-release-new.yaml")
			err = os.WriteFile(newBSFile, []byte(newBSYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(newBSFile)

			cmd = exec.Command("kubectl", "apply", "-f", newBSFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying new BatchSandbox gets allocated correctly")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("2"))

				cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, _ := utils.Run(cmd)
				allocated := 0
				fmt.Sscanf(allocatedStr, "%d", &allocated)
				g.Expect(allocated).To(Equal(2))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", newBatchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should panic and exit when Recover fails due to malformed annotation", func() {
			const poolName = "test-pool-recover-fail"
			const batchSandboxName = "test-bs-malformed"
			const testNamespace = "default"

			By("first stopping controller to prevent early sync.Once execution")
			cmd := exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
				"--replicas=0", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for controller pod to terminate")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).ShouldNot(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("creating a Pool (while controller is stopped)")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-recover-fail.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a BatchSandbox with malformed alloc-status annotation (while controller is stopped)")
			bsYAML, err := renderTemplate("testdata/batchsandbox-malformed-annotation.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-malformed.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying malformed annotation is set correctly")
			cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
				"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
			annoOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(annoOutput).To(ContainSubstring("invalid json"), "BatchSandbox should have malformed annotation")

			By("verifying BatchSandbox has poolRef set")
			cmd = exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
				"-o", "jsonpath={.spec.poolRef}")
			poolRefOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(poolRefOutput).To(Equal(poolName), "BatchSandbox should have poolRef set to %s", poolName)

			By("scaling up controller-manager - should exit due to malformed annotation during Recover")
			cmd = exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
				"--replicas=1", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for controller to process Pool, attempt Recover, and exit")
			// Wait long enough for:
			// 1. Container to start
			// 2. Leader election to complete
			// 3. Pool controller to reconcile and call Schedule -> checkRecovery -> Recover
			// 4. Recover to fail and call os.Exit(1)
			// 5. Container to restart
			time.Sleep(60 * time.Second)

			By("verifying container restarted and exited with code 1")
			// Get pod status to check restartCount and exit code
			cmd = exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "json")
			podJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var podList struct {
				Items []struct {
					Status struct {
						ContainerStatuses []struct {
							RestartCount int `json:"restartCount"`
							LastState    struct {
								Terminated *struct {
									ExitCode int    `json:"exitCode"`
									Reason   string `json:"reason"`
								} `json:"terminated"`
							} `json:"lastState"`
							State struct {
								Waiting *struct {
									Reason  string `json:"reason"`
									Message string `json:"message"`
								} `json:"waiting"`
							} `json:"state"`
						} `json:"containerStatuses"`
					} `json:"status"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(podJSON), &podList)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(podList.Items)).To(BeNumerically(">", 0), "Should have at least one controller pod")

			// Find a container that has restarted
			foundRestarted := false
			for _, pod := range podList.Items {
				for _, container := range pod.Status.ContainerStatuses {
					if container.RestartCount > 0 {
						foundRestarted = true
						// Verify the last termination had exit code 1
						if container.LastState.Terminated != nil {
							Expect(container.LastState.Terminated.ExitCode).To(Equal(1),
								"Container should exit with code 1 due to Recover failure")
						}
						break
					}
				}
				if foundRestarted {
					break
				}
			}
			Expect(foundRestarted).To(BeTrue(), "Container should have restarted due to Recover failure")

			By("cleaning up - deleting malformed BatchSandbox first")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("restarting controller after cleanup - should succeed now")
			err = restartController()
			Expect(err).NotTo(HaveOccurred())

			By("cleaning up pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Pool Recycle", Label("Pool"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should keep pods alive when BatchSandbox is released with Noop recycle strategy", func() {
			const poolName = "test-pool-recycle-noop"
			const batchSandboxName = "test-bs-recycle-noop"
			const testNamespace = "default"
			const replicas = 1

			By("creating a Pool with Noop recycle strategy")
			poolYAML, err := renderTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"RecycleType":  "Noop",
				"Command":      `["sleep", "3600"]`,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready with at least poolMin pods running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(totalStr, "%d", &total)
				g.Expect(total).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox that allocates from the pool")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to allocate pods from the pool")
			var allocatedPodNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedPodNames = allocStatus.Pods
				g.Expect(allocatedPodNames).To(HaveLen(replicas))
			}, 2*time.Minute).Should(Succeed())

			By("recording UIDs of the allocated pods before release")
			podUIDsBefore := make(map[string]string)
			for _, podName := range allocatedPodNames {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.uid}")
				uid, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				podUIDsBefore[podName] = uid
			}

			By("deleting the BatchSandbox to trigger Noop recycle")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying allocated pods are NOT deleted with Noop recycle strategy")
			Consistently(func(g Gomega) {
				for _, podName := range allocatedPodNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "pod %s should still exist after Noop recycle", podName)
					g.Expect(output).To(BeEmpty(), "pod %s should not be terminating with Noop recycle strategy", podName)
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying pods keep the same UID (not recreated) with Noop recycle strategy")
			for _, podName := range allocatedPodNames {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.uid}")
				uidAfter, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(uidAfter).To(Equal(podUIDsBefore[podName]),
					"pod %s should have the same UID after Noop recycle (not recreated)", podName)
			}

			By("verifying the pool allocation count is decreased after releasing the BatchSandbox")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				allocatedStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				if allocatedStr != "" {
					fmt.Sscanf(allocatedStr, "%d", &allocated)
				}
				g.Expect(allocated).To(Equal(0), "Pool allocated count should be 0 after BatchSandbox deletion")
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying the pool available count increases after Noop recycle completes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				availableStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				available := 0
				if availableStr != "" {
					fmt.Sscanf(availableStr, "%d", &available)
				}
				g.Expect(available).To(BeNumerically(">=", 1),
					"Pool available count should be >= 1 after Noop recycle completes")
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying a new BatchSandbox can be allocated from the pool after Noop recycle")
			const newBatchSandboxNameNoop = "test-bs-recycle-noop-new"
			newBSYAMLNoop, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": newBatchSandboxNameNoop,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			newBSFileNoop := filepath.Join("/tmp", newBatchSandboxNameNoop+".yaml")
			err = os.WriteFile(newBSFileNoop, []byte(newBSYAMLNoop), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(newBSFileNoop)

			cmd = exec.Command("kubectl", "apply", "-f", newBSFileNoop)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var newAllocatedPods []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxNameNoop, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"), "New BatchSandbox should get 1 allocated pod")

				cmd = exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxNameNoop, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				newAllocatedPods = allocStatus.Pods
				g.Expect(newAllocatedPods).To(HaveLen(1))
			}, 2*time.Minute).Should(Succeed())

			By("verifying the original pods are still alive after Noop recycle (core Noop guarantee)")
			for _, podName := range allocatedPodNames {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.uid}")
				currentUID, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "original pod %s should still exist after Noop recycle", podName)
				Expect(currentUID).To(Equal(podUIDsBefore[podName]),
					"original pod %s should have the same UID (not recreated by Noop recycle)", podName)
			}

			By("verifying new BatchSandbox got a valid pod from the pool")
			Expect(newAllocatedPods).To(HaveLen(1), "New BatchSandbox should have exactly 1 allocated pod")
			_ = newAllocatedPods

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", newBatchSandboxNameNoop, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should compare Noop vs Delete recycle strategy behavior when BatchSandbox is released", func() {
			const poolNameNoop = "test-pool-compare-noop"
			const poolNameDelete = "test-pool-compare-delete"
			const bsNameNoop = "test-bs-compare-noop"
			const bsNameDelete = "test-bs-compare-delete"
			const testNamespace = "default"
			const replicas = 1

			By("creating two pools: one with Noop strategy, one with Delete strategy")
			noopPoolYAML, err := renderTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolNameNoop,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
				"RecycleType":  "Noop",
				"Command":      `["sleep", "3600"]`,
			})
			Expect(err).NotTo(HaveOccurred())

			deletePoolYAML, err := renderTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolNameDelete,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
				"RecycleType":  "Delete",
				"Command":      `["sleep", "3600"]`,
			})
			Expect(err).NotTo(HaveOccurred())

			noopPoolFile := filepath.Join("/tmp", poolNameNoop+".yaml")
			err = os.WriteFile(noopPoolFile, []byte(noopPoolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(noopPoolFile)

			deletePoolFile := filepath.Join("/tmp", poolNameDelete+".yaml")
			err = os.WriteFile(deletePoolFile, []byte(deletePoolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(deletePoolFile)

			cmd := exec.Command("kubectl", "apply", "-f", noopPoolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "apply", "-f", deletePoolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for both pools to be ready")
			Eventually(func(g Gomega) {
				for _, pName := range []string{poolNameNoop, poolNameDelete} {
					cmd := exec.Command("kubectl", "get", "pool", pName, "-n", testNamespace,
						"-o", "jsonpath={.status.total}")
					totalStr, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					total := 0
					fmt.Sscanf(totalStr, "%d", &total)
					g.Expect(total).To(BeNumerically(">=", 1))
				}
			}, 3*time.Minute).Should(Succeed())

			By("creating BatchSandboxes for both pools")
			for _, bsInfo := range []struct {
				name     string
				poolName string
			}{
				{bsNameNoop, poolNameNoop},
				{bsNameDelete, poolNameDelete},
			} {
				bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
					"BatchSandboxName": bsInfo.name,
					"Namespace":        testNamespace,
					"Replicas":         replicas,
					"PoolName":         bsInfo.poolName,
				})
				Expect(err).NotTo(HaveOccurred())

				bsFile := filepath.Join("/tmp", bsInfo.name+".yaml")
				err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
				Expect(err).NotTo(HaveOccurred())
				defer os.Remove(bsFile)

				cmd := exec.Command("kubectl", "apply", "-f", bsFile)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("waiting for both BatchSandboxes to allocate pods")
			noopAllocatedPods := []string{}
			deleteAllocatedPods := []string{}

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsNameNoop, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				noopAllocJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(noopAllocJSON).NotTo(BeEmpty())
				var noopAlloc struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(noopAllocJSON), &noopAlloc)
				g.Expect(err).NotTo(HaveOccurred())
				noopAllocatedPods = noopAlloc.Pods
				g.Expect(noopAllocatedPods).To(HaveLen(replicas))

				cmd = exec.Command("kubectl", "get", "batchsandbox", bsNameDelete, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				deleteAllocJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deleteAllocJSON).NotTo(BeEmpty())
				var deleteAlloc struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(deleteAllocJSON), &deleteAlloc)
				g.Expect(err).NotTo(HaveOccurred())
				deleteAllocatedPods = deleteAlloc.Pods
				g.Expect(deleteAllocatedPods).To(HaveLen(replicas))
			}, 2*time.Minute).Should(Succeed())

			By("deleting both BatchSandboxes to trigger their respective recycle strategies")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsNameNoop, "-n", testNamespace, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsNameDelete, "-n", testNamespace, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying: Noop pods remain alive, Delete pods are terminated")
			Eventually(func(g Gomega) {
				for _, podName := range noopAllocatedPods {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "Noop pod %s should still exist", podName)
					g.Expect(output).To(BeEmpty(),
						"Noop pod %s should NOT be terminating (Noop means no action)", podName)
				}

				allDeletePodsGoneOrTerminating := true
				for _, podName := range deleteAllocatedPods {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					if err != nil && strings.Contains(err.Error(), "not found") {
						continue
					}
					g.Expect(err).NotTo(HaveOccurred())
					if output == "" {
						allDeletePodsGoneOrTerminating = false
					}
				}
				g.Expect(allDeletePodsGoneOrTerminating).To(BeTrue(),
					"Delete strategy pods should be terminating or already deleted")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("cleaning up both pools")
			cmd = exec.Command("kubectl", "delete", "pool", poolNameNoop, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolNameDelete, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should delete pods when BatchSandbox is released with Delete recycle strategy", func() {
			const poolName = "test-pool-recycle-delete"
			const batchSandboxName = "test-bs-recycle-delete"
			const testNamespace = "default"
			const replicas = 1

			By("creating a Pool with explicit Delete recycle strategy")
			poolYAML, err := renderTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"RecycleType":  "Delete",
				"Command":      `["sleep", "3600"]`,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox that allocates from the pool")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to allocate pods")
			var allocatedPodNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedPodNames = allocStatus.Pods
				g.Expect(allocatedPodNames).To(HaveLen(replicas))
			}, 2*time.Minute).Should(Succeed())

			By("deleting the BatchSandbox to trigger recycle")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying allocated pods are deleted with Delete recycle strategy")
			Eventually(func(g Gomega) {
				for _, podName := range allocatedPodNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					if err != nil && strings.Contains(err.Error(), "not found") {
						continue // pod already gone
					}
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).NotTo(BeEmpty(), "pod %s should be terminating with Delete recycle strategy", podName)
				}
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should restart containers when BatchSandbox is released with Restart recycle strategy", func() {
			const poolName = "test-pool-recycle-restart"
			const batchSandboxName = "test-bs-recycle-restart"
			const testNamespace = "default"
			const replicas = 1

			By("creating a Pool with Restart recycle strategy")
			poolYAML, err := renderTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"RecycleType":  "Restart",
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(totalStr).NotTo(BeEmpty())
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox that allocates from the pool")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to allocate pods")
			var allocatedPodNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				allocatedPodNames = allocStatus.Pods
				g.Expect(allocatedPodNames).To(HaveLen(replicas))
			}, 2*time.Minute).Should(Succeed())

			By("recording pod container restart counts before recycle")
			restartCountsBefore := make(map[string]int32)
			for _, podName := range allocatedPodNames {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				var count int32
				fmt.Sscanf(output, "%d", &count)
				restartCountsBefore[podName] = count
			}

			By("deleting the BatchSandbox to trigger recycle")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods are NOT deleted with Restart recycle strategy")
			Consistently(func(g Gomega) {
				for _, podName := range allocatedPodNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "pod %s should still exist", podName)
					g.Expect(output).To(BeEmpty(), "pod %s should not be terminating with Restart recycle strategy", podName)
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying containers are restarted (restart count increases)")
			Eventually(func(g Gomega) {
				for _, podName := range allocatedPodNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					var currentCount int32
					fmt.Sscanf(output, "%d", &currentCount)
					beforeCount := restartCountsBefore[podName]
					g.Expect(currentCount).To(BeNumerically(">", beforeCount),
						"pod %s container restart count should increase after Restart recycle", podName)
				}
			}, 2*time.Minute).Should(Succeed())

			By("verifying pods become available again in the pool")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				availableStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				available := 0
				if availableStr != "" {
					fmt.Sscanf(availableStr, "%d", &available)
				}
				g.Expect(available).To(BeNumerically(">=", 1), "Pool should have at least 1 available pod after restart recycle")
			}, 2*time.Minute).Should(Succeed())

			By("verifying a new BatchSandbox can be allocated from the recycled pool")
			const newBatchSandboxName = "test-bs-recycle-restart-new"
			newBSYAML, err := renderTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": newBatchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			})
			Expect(err).NotTo(HaveOccurred())

			newBSFile := filepath.Join("/tmp", newBatchSandboxName+".yaml")
			err = os.WriteFile(newBSFile, []byte(newBSYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(newBSFile)

			cmd = exec.Command("kubectl", "apply", "-f", newBSFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", newBatchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", newBatchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Pool Allocator Integrity", Label("Pool"), func() {
		const testNamespace = "default"

		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		// Case 1: Concurrent allocation consistency
		// Creates multiple BatchSandboxes simultaneously with varied replica counts and verifies
		// no duplicate pod allocation and pool.status.allocated == sum of all replicas.
		It("should allocate pods without duplication under concurrent BatchSandbox creation", func() {
			const poolName = "test-pai-concurrent"

			// Each BS gets a distinct replica count to exercise the allocator under varied demand.
			type bsSpec struct {
				name     string
				replicas int
			}
			bsSpecs := []bsSpec{
				{"test-pai-concurrent-bs1", 1},
				{"test-pai-concurrent-bs2", 3},
				{"test-pai-concurrent-bs3", 2},
			}
			totalReplicas := 0
			for _, s := range bsSpecs {
				totalReplicas += s.replicas
			}

			By(fmt.Sprintf("creating a Pool with sufficient capacity (need %d pods)", totalReplicas))
			poolMax := totalReplicas + 3 // headroom for buffer
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      poolMax,
				"PoolMin":      totalReplicas,
			}, poolName+".yaml")

			By("waiting for pool to be stable")
			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By(fmt.Sprintf("creating %d BatchSandboxes concurrently with replicas [1,3,2]", len(bsSpecs)))
			for _, s := range bsSpecs {
				applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
					"BatchSandboxName": s.name,
					"Namespace":        testNamespace,
					"Replicas":         s.replicas,
					"PoolName":         poolName,
				}, s.name+".yaml")
			}

			By("waiting for all BatchSandboxes to reach their requested replica count")
			Eventually(func(g Gomega) {
				for _, s := range bsSpecs {
					cmd := exec.Command("kubectl", "get", "batchsandbox", s.name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal(fmt.Sprintf("%d", s.replicas)),
						"bs %s should have %d allocated pods", s.name, s.replicas)
				}
			}, 3*time.Minute).Should(Succeed())

			By("verifying no duplicate pod allocation across all BatchSandboxes")
			allAllocated := make(map[string]string) // podName -> bsName
			for _, s := range bsSpecs {
				pods := getAllocatedPods(s.name, testNamespace, s.replicas)
				for _, pod := range pods {
					existing, dup := allAllocated[pod]
					Expect(dup).To(BeFalse(), "pod %s is duplicated between %s and %s", pod, existing, s.name)
					allAllocated[pod] = s.name
				}
			}

			By(fmt.Sprintf("verifying pool.status.allocated == total replicas (%d)", totalReplicas))
			cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			allocStr, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			alloc := 0
			fmt.Sscanf(allocStr, "%d", &alloc)
			Expect(alloc).To(Equal(totalReplicas),
				"pool.status.allocated should equal sum of all BS replicas")

			By("verifying pool invariant available+allocated==total")
			waitPoolStable(poolName, testNamespace, time.Minute)

			By("cleaning up")
			for _, s := range bsSpecs {
				cmd := exec.Command("kubectl", "delete", "batchsandbox", s.name, "-n", testNamespace, "--wait=false")
				_, _ = utils.Run(cmd)
			}
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 2: Allocate-release-reallocate cycle with Noop recycle (pod reuse, no memory leak)
		It("should reuse pods across allocate-release cycles without memory leak (Noop)", func() {
			const poolName = "test-pai-cycle-noop"

			By("creating a Pool with Noop recycle strategy")
			applyTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
				"RecycleType":  "Noop",
				"Command":      `["sleep", "3600"]`,
			}, poolName+".yaml")

			By("waiting for pool to be stable")
			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			var round1Pods []string
			for round := 1; round <= 2; round++ {
				bsName := fmt.Sprintf("test-pai-cycle-bs-r%d", round)

				By(fmt.Sprintf("round %d: allocating via BatchSandbox", round))
				applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
					"BatchSandboxName": bsName,
					"Namespace":        testNamespace,
					"Replicas":         2,
					"PoolName":         poolName,
				}, bsName+".yaml")

				By(fmt.Sprintf("round %d: waiting for allocation", round))
				allocPods := getAllocatedPods(bsName, testNamespace, 2)

				if round == 1 {
					round1Pods = allocPods
				} else {
					By("round 2: verifying pods are reused from round 1 (Noop does not delete pods)")
					round1Set := make(map[string]bool)
					for _, p := range round1Pods {
						round1Set[p] = true
					}
					reuseCount := 0
					for _, p := range allocPods {
						if round1Set[p] {
							reuseCount++
						}
					}
					Expect(reuseCount).To(BeNumerically(">", 0),
						"at least one pod should be reused in round 2 with Noop strategy")
				}

				By(fmt.Sprintf("round %d: releasing via BatchSandbox deletion", round))
				cmd := exec.Command("kubectl", "delete", "batchsandbox", bsName, "-n", testNamespace)
				_, _ = utils.Run(cmd)

				By(fmt.Sprintf("round %d: waiting for pool.status.allocated to return to 0", round))
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					allocated := 0
					fmt.Sscanf(out, "%d", &allocated)
					g.Expect(allocated).To(Equal(0))
				}, 2*time.Minute).Should(Succeed())
			}

			By("verifying pool.status.total did not exceed poolMax after two cycles")
			cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.total}")
			totalStr, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			total := 0
			fmt.Sscanf(totalStr, "%d", &total)
			Expect(total).To(BeNumerically("<=", 4), "pool total should not exceed poolMax")

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 3: PoolMax hard constraint
		// Verifies that total pods never exceed PoolMax even when demand exceeds capacity.
		It("should enforce PoolMax and not over-provision pods", func() {
			const poolName = "test-pai-poolmax"

			By("creating a Pool with tight PoolMax")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
			}, poolName+".yaml")

			By("waiting for pool to be stable")
			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("creating BS-A that consumes 3 pods")
			const bsA = "test-pai-poolmax-bs-a"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsA,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolName,
			}, bsA+".yaml")

			By("waiting for BS-A to be fully allocated")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsA, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("3"))
			}, 3*time.Minute).Should(Succeed())

			By("creating BS-B that also requests 3 pods (demand exceeds remaining capacity)")
			const bsB = "test-pai-poolmax-bs-b"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsB,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolName,
			}, bsB+".yaml")

			By("verifying pool.status.total never exceeds PoolMax")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(out, "%d", &total)
				g.Expect(total).To(BeNumerically("<=", 4), "pool total must not exceed PoolMax=4")
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying BS-A allocation is not stolen")
			cmd := exec.Command("kubectl", "get", "batchsandbox", bsA, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("3"), "BS-A should still have 3 allocated pods")

			By("deleting BS-A and waiting for BS-B to be fully satisfied")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsA, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsB, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("3"), "BS-B should be fully satisfied after BS-A is released")
			}, 3*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsB, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 4: Recover correctly restores allocated count after controller restart.
		// Strategy: allocate pods → scale-down controller → scale-up → verify allocated count
		// is restored from alloc-status annotation without double-counting or data loss.
		// Also verifies that alloc-released pods are NOT counted as allocated after Recover.
		It("should restore allocated count correctly after controller restart (Recover)", func() {
			const poolName = "test-pai-recover"
			const bsA = "test-pai-recover-bs-a"
			const bsB = "test-pai-recover-bs-b"

			By("creating a Pool")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
			}, poolName+".yaml")
			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("allocating 2 pods via two BatchSandboxes")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsA,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsA+".yaml")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsB,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsB+".yaml")
			// wait for both BS to be allocated
			Eventually(func(g Gomega) {
				for _, name := range []string{bsA, bsB} {
					cmd := exec.Command("kubectl", "get", "batchsandbox", name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("1"), "bs %s should have 1 allocated pod", name)
				}
			}, 3*time.Minute).Should(Succeed())

			By("reading pool.status.allocated before restart (should be 2)")
			cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			allocBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			allocBeforeInt := 0
			fmt.Sscanf(allocBefore, "%d", &allocBeforeInt)
			Expect(allocBeforeInt).To(Equal(2), "should have 2 allocated pods before restart")

			By("scaling down controller")
			cmd = exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
				"--replicas=0", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("scaling up controller to trigger Recover")
			cmd = exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
				"--replicas=1", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying pool.status.allocated is restored to 2 after Recover")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(out, "%d", &allocated)
				g.Expect(allocated).To(Equal(2), "Recover must restore allocated count from annotations")
			}, 30*time.Second).Should(Succeed())

			By("verifying a new BatchSandbox can still allocate after Recover (no duplicate)")
			const bsC = "test-pai-recover-bs-c"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsC,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsC+".yaml")
			allocPodsC := getAllocatedPods(bsC, testNamespace, 1)
			allocPodsA := getAllocatedPods(bsA, testNamespace, 1)
			allocPodsB := getAllocatedPods(bsB, testNamespace, 1)
			// verify bsC got a different pod (no duplicate)
			existing := make(map[string]bool)
			for _, p := range append(allocPodsA, allocPodsB...) {
				existing[p] = true
			}
			for _, p := range allocPodsC {
				Expect(existing[p]).To(BeFalse(), "Recover must not allow duplicate allocation of pod %s", p)
			}

			By("cleaning up")
			for _, name := range []string{bsA, bsB, bsC} {
				cmd = exec.Command("kubectl", "delete", "batchsandbox", name, "-n", testNamespace, "--wait=false")
				_, _ = utils.Run(cmd)
			}
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 5: Orphan pod GC
		// When an allocated pod is forcefully deleted, the allocator must release the in-memory
		// entry and the pool must self-heal by replenishing pods.
		It("should GC orphan pods and self-heal after an allocated pod is forcefully deleted", func() {
			const poolName = "test-pai-orphan-gc"
			const bsName = "test-pai-orphan-gc-bs"

			By("creating a Pool")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
			}, poolName+".yaml")

			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("allocating 1 pod via BatchSandbox")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsName+".yaml")
			allocPods := getAllocatedPods(bsName, testNamespace, 1)
			orphanPod := allocPods[0]

			By("deleting the BatchSandbox first to allow GC to proceed cleanly")
			cmd := exec.Command("kubectl", "delete", "batchsandbox", bsName, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("waiting for pool.status.allocated to return to 0")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(out, "%d", &allocated)
				g.Expect(allocated).To(Equal(0))
			}, 2*time.Minute).Should(Succeed())

			By("forcefully deleting the previously allocated pod to simulate node failure")
			cmd = exec.Command("kubectl", "delete", "pod", orphanPod, "-n", testNamespace,
				"--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			By("verifying pool self-heals and reaches PoolMin again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				running := strings.Fields(out)
				g.Expect(len(running)).To(BeNumerically(">=", 2), "pool should recover to at least poolMin=2")
			}, 3*time.Minute).Should(Succeed())

			By("verifying a new BatchSandbox can allocate after orphan GC")
			const bsNew = "test-pai-orphan-gc-bs2"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsNew,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsNew+".yaml")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsNew, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsNew, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 6: Rolling upgrade continuity
		// During pool template rolling update, already-allocated pods must not be rebuilt
		// and new BatchSandboxes must still be allocatable.
		It("should maintain allocation continuity during rolling pool upgrade", func() {
			const poolName = "test-pai-rolling"
			const bsA = "test-pai-rolling-bs-a"

			By("creating a Pool with update strategy")
			applyTemplate("testdata/pool-with-update-strategy.yaml", map[string]interface{}{
				"PoolName":       poolName,
				"SandboxImage":   utils.SandboxImage,
				"Namespace":      testNamespace,
				"BufferMax":      3,
				"BufferMin":      2,
				"PoolMax":        6,
				"PoolMin":        3,
				"MaxUnavailable": 2,
				"EnvValue":       "v1",
			}, poolName+".yaml")

			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("allocating 2 pods via BS-A before upgrade")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsA,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolName,
			}, bsA+".yaml")
			allocPodsBeforeUpgrade := getAllocatedPods(bsA, testNamespace, 2)

			By("recording pod UIDs before upgrade")
			podUIDsBefore := make(map[string]string)
			for _, podName := range allocPodsBeforeUpgrade {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.uid}")
				uid, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				podUIDsBefore[podName] = uid
			}

			By("triggering pool template upgrade to v2")
			applyTemplate("testdata/pool-with-update-strategy.yaml", map[string]interface{}{
				"PoolName":       poolName,
				"SandboxImage":   utils.SandboxImage,
				"Namespace":      testNamespace,
				"BufferMax":      3,
				"BufferMin":      2,
				"PoolMax":        6,
				"PoolMin":        3,
				"MaxUnavailable": 2,
				"EnvValue":       "v2",
			}, poolName+"-v2.yaml")

			By("verifying allocated pods are not rebuilt during upgrade (UIDs unchanged)")
			Consistently(func(g Gomega) {
				for _, podName := range allocPodsBeforeUpgrade {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.uid}")
					uid, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(uid).To(Equal(podUIDsBefore[podName]),
						"allocated pod %s UID changed during upgrade", podName)
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying a new BatchSandbox can still be allocated during upgrade")
			const bsB = "test-pai-rolling-bs-b"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsB,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsB+".yaml")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsB, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"))
			}, 3*time.Minute).Should(Succeed())

			By("releasing allocated pods (delete BS-A) and waiting for upgrade to complete")
			// Rolling upgrade skips allocated pods; we must release them first.
			cmd := exec.Command("kubectl", "delete", "batchsandbox", bsA, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			// Then delete bs-B as well since we no longer need it for the upgrade check.
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsB, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.updated}")
				updatedStr, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				cmd2 := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				totalStr, err := utils.Run(cmd2)
				g.Expect(err).NotTo(HaveOccurred())
				updated, total := 0, 0
				fmt.Sscanf(updatedStr, "%d", &updated)
				fmt.Sscanf(totalStr, "%d", &total)
				g.Expect(total).To(BeNumerically(">", 0))
				g.Expect(updated).To(Equal(total), "all pods should be on the new revision after releasing allocated pods")
			}, 5*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 7: Evict + allocate race
		// Allocated pods with evict label must NOT be evicted; idle pods with evict label must be evicted.
		It("should evict idle pods but protect allocated pods when all pods are marked for eviction", func() {
			const poolName = "test-pai-evict-alloc"
			const bsName = "test-pai-evict-alloc-bs"

			By("creating a Pool")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      4,
				"PoolMin":      2,
			}, poolName+".yaml")

			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("allocating 1 pod via BatchSandbox")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolName,
			}, bsName+".yaml")
			allocPods := getAllocatedPods(bsName, testNamespace, 1)

			By("collecting all pool pods and marking them all for eviction")
			cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
				"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
				"-o", "jsonpath={.items[*].metadata.name}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			allPods := strings.Fields(out)
			for _, pod := range allPods {
				cmd := exec.Command("kubectl", "label", "pod", pod, "-n", testNamespace,
					"pool.opensandbox.io/evict=true", "--overwrite")
				_, _ = utils.Run(cmd)
			}

			By("verifying allocated pods are never deleted")
			Consistently(func(g Gomega) {
				for _, pod := range allocPods {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "allocated pod %s should exist", pod)
					g.Expect(out).To(BeEmpty(), "allocated pod %s must not be terminating", pod)
				}
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying idle pods are evicted")
			allocSet := make(map[string]bool)
			for _, p := range allocPods {
				allocSet[p] = true
			}
			var idlePods []string
			for _, p := range allPods {
				if !allocSet[p] {
					idlePods = append(idlePods, p)
				}
			}
			if len(idlePods) > 0 {
				Eventually(func(g Gomega) {
					for _, pod := range idlePods {
						cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
							"-o", "jsonpath={.metadata.deletionTimestamp}")
						out, err := utils.Run(cmd)
						if err != nil && strings.Contains(err.Error(), "not found") {
							return // already gone is also acceptable
						}
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(out).NotTo(BeEmpty(), "idle pod %s should be terminating", pod)
					}
				}, 30*time.Second, 2*time.Second).Should(Succeed())
			}

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsName, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 8: Multi-pool namespace isolation
		// Two pools in the same namespace must not share pods or interfere with each other's allocator state.
		It("should isolate allocator state between two pools in the same namespace", func() {
			const poolA = "test-pai-isolation-a"
			const poolB = "test-pai-isolation-b"
			const bsA = "test-pai-isolation-bs-a"
			const bsB = "test-pai-isolation-bs-b"

			By("creating Pool-A")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolA,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      3,
				"PoolMin":      2,
			}, poolA+".yaml")

			By("creating Pool-B")
			applyTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolB,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      3,
				"PoolMin":      2,
			}, poolB+".yaml")

			waitPoolStable(poolA, testNamespace, 3*time.Minute)
			waitPoolStable(poolB, testNamespace, 3*time.Minute)

			By("creating BS-A referencing Pool-A")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsA,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolA,
			}, bsA+".yaml")

			By("creating BS-B referencing Pool-B")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsB,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolB,
			}, bsB+".yaml")

			By("waiting for both BatchSandboxes to be fully allocated")
			Eventually(func(g Gomega) {
				for _, name := range []string{bsA, bsB} {
					cmd := exec.Command("kubectl", "get", "batchsandbox", name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("2"), "bs %s should have 2 allocated pods", name)
				}
			}, 3*time.Minute).Should(Succeed())

			By("verifying pods allocated to BS-A carry Pool-A label, and BS-B carry Pool-B label")
			podsA := getAllocatedPods(bsA, testNamespace, 2)
			podsB := getAllocatedPods(bsB, testNamespace, 2)
			for _, pod := range podsA {
				cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
					"-o", "jsonpath={.metadata.labels.sandbox\\.opensandbox\\.io/pool-name}")
				out, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal(poolA), "pod %s should belong to Pool-A", pod)
			}
			for _, pod := range podsB {
				cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
					"-o", "jsonpath={.metadata.labels.sandbox\\.opensandbox\\.io/pool-name}")
				out, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal(poolB), "pod %s should belong to Pool-B", pod)
			}

			By("verifying no pod intersection between Pool-A and Pool-B allocations")
			podSetA := make(map[string]bool)
			for _, p := range podsA {
				podSetA[p] = true
			}
			for _, p := range podsB {
				Expect(podSetA).NotTo(HaveKey(p), "pod %s is shared between Pool-A and Pool-B", p)
			}

			By("deleting Pool-A and verifying Pool-B allocation is unaffected")
			cmd := exec.Command("kubectl", "delete", "batchsandbox", bsA, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolA, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", bsB, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("2"), "Pool-B allocation should remain 2 after Pool-A deletion")
			}, 20*time.Second, 2*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsB, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolB, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 9: Staggered multi-BS release consistency
		// Three BatchSandboxes hold varied replica counts (1, 3, 2).
		// They are released in three distinct waves:
		//   wave-1: BS-A (1 pod) deleted synchronously
		//   wave-2: BS-B (3 pods) deleted asynchronously, wait for pool to settle
		//   wave-3: BS-C (2 pods) deleted last
		// After each wave, pool.status.allocated must drop by exactly the number of pods
		// released. After all waves, allocated==0 and pool self-heals to PoolMin.
		It("should maintain correct allocated count through staggered multi-BS release", func() {
			const poolName = "test-pai-stagger-release"
			const bsA = "test-pai-stagger-bs-a" // replicas=1
			const bsB = "test-pai-stagger-bs-b" // replicas=3
			const bsC = "test-pai-stagger-bs-c" // replicas=2
			const poolMin = 3

			By("creating a Noop-recycle Pool (pods survive release, maximising reuse stress)")
			applyTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      9,
				"PoolMin":      poolMin,
				"RecycleType":  "Noop",
				"Command":      `["sleep", "3600"]`,
			}, poolName+".yaml")
			waitPoolStable(poolName, testNamespace, 3*time.Minute)

			By("allocating BS-A(1), BS-B(3), BS-C(2) concurrently")
			for _, spec := range []struct {
				name     string
				replicas int
			}{
				{bsA, 1},
				{bsB, 3},
				{bsC, 2},
			} {
				applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
					"BatchSandboxName": spec.name,
					"Namespace":        testNamespace,
					"Replicas":         spec.replicas,
					"PoolName":         poolName,
				}, spec.name+".yaml")
			}

			By("waiting for all three BS to be fully allocated (total 6 pods)")
			Eventually(func(g Gomega) {
				for _, spec := range []struct {
					name     string
					replicas int
				}{{bsA, 1}, {bsB, 3}, {bsC, 2}} {
					cmd := exec.Command("kubectl", "get", "batchsandbox", spec.name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal(fmt.Sprintf("%d", spec.replicas)),
						"bs %s should have %d allocated pods", spec.name, spec.replicas)
				}
			}, 3*time.Minute).Should(Succeed())

			// Snapshot the exact pod sets per BS before any release.
			allPodsA := getAllocatedPods(bsA, testNamespace, 1)
			allPodsB := getAllocatedPods(bsB, testNamespace, 3)
			allPodsC := getAllocatedPods(bsC, testNamespace, 2)
			totalAllocated := len(allPodsA) + len(allPodsB) + len(allPodsC) // 6

			By("confirming no cross-BS pod overlap before any release")
			allPods := make(map[string]string)
			for _, p := range allPodsA {
				allPods[p] = bsA
			}
			for _, p := range allPodsB {
				_, dup := allPods[p]
				Expect(dup).To(BeFalse(), "pod %s appears in both %s and %s", p, allPods[p], bsB)
				allPods[p] = bsB
			}
			for _, p := range allPodsC {
				_, dup := allPods[p]
				Expect(dup).To(BeFalse(), "pod %s appears in both %s and %s", p, allPods[p], bsC)
				allPods[p] = bsC
			}

			// ── Wave 1: release BS-A (1 pod) synchronously ──────────────────────────
			By("wave-1: deleting BS-A (1 pod) synchronously")
			cmd := exec.Command("kubectl", "delete", "batchsandbox", bsA, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			totalAllocated -= len(allPodsA)

			By(fmt.Sprintf("wave-1: pool.status.allocated must drop to %d", totalAllocated))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(out, "%d", &allocated)
				g.Expect(allocated).To(Equal(totalAllocated),
					"after wave-1 allocated should be %d", totalAllocated)
			}, 2*time.Minute).Should(Succeed())

			// ── Wave 2: release BS-B (3 pods) asynchronously, then wait ─────────────
			By("wave-2: deleting BS-B (3 pods) with --wait=false then waiting for settle")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsB, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			totalAllocated -= len(allPodsB)

			By(fmt.Sprintf("wave-2: pool.status.allocated must drop to %d", totalAllocated))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(out, "%d", &allocated)
				g.Expect(allocated).To(Equal(totalAllocated),
					"after wave-2 allocated should be %d", totalAllocated)
			}, 2*time.Minute).Should(Succeed())

			// ── Wave 3: release BS-C (2 pods) synchronously ─────────────────────────
			By("wave-3: deleting BS-C (2 pods) synchronously")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsC, "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("wave-3: pool.status.allocated must reach 0")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				allocated := 0
				fmt.Sscanf(out, "%d", &allocated)
				g.Expect(allocated).To(Equal(0), "all pods should be released after wave-3")
			}, 2*time.Minute).Should(Succeed())

			By("verifying pool self-heals back to PoolMin after all releases")
			waitPoolStable(poolName, testNamespace, 2*time.Minute)
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.total}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			total := 0
			fmt.Sscanf(out, "%d", &total)
			Expect(total).To(BeNumerically(">=", poolMin),
				"pool should recover to at least PoolMin=%d after all releases", poolMin)

			By("verifying a new BatchSandbox can still allocate correctly after all staggered releases")
			const bsNew = "test-pai-stagger-bs-new"
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsNew,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolName,
			}, bsNew+".yaml")
			newPods := getAllocatedPods(bsNew, testNamespace, 2)
			// The 2 new pods must not overlap with the original 6 allocated pods
			// (Noop keeps old pods alive but they should be re-usable, not double-allocated)
			cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			out, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			newAlloc := 0
			fmt.Sscanf(out, "%d", &newAlloc)
			Expect(newAlloc).To(Equal(len(newPods)), "pool allocated should equal new BS replica count")

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsNew, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		// Case 10: Three-pool mixed recycle strategy under interleaved alloc/release
		// Three pools run concurrently, each with a different recycle strategy:
		//   Pool-Noop   (Noop)   – released pods stay alive, become available again
		//   Pool-Delete (Delete) – released pods are terminated
		//   Pool-Restart(Restart)– released pods get container-restarted, become available again
		//
		// Allocation and release are interleaved across all three pools:
		//   Phase-1: allocate BS per pool simultaneously (replicas: 2, 1, 3)
		//   Phase-2: release pool-Noop BS and pool-Delete BS concurrently, while pool-Restart BS is still held
		//   Phase-3: allocate a second BS against pool-Noop and pool-Delete while pool-Restart is releasing
		//   Phase-4: verify per-pool invariants and cross-pool isolation
		It("should maintain per-pool integrity with Noop/Delete/Restart recycle under interleaved alloc-release", func() {
			const (
				poolNoop    = "test-pai-mixed-noop"
				poolDelete  = "test-pai-mixed-delete"
				poolRestart = "test-pai-mixed-restart"
			)

			// ── Setup: create all three pools ──────────────────────────────────────
			By("creating Pool-Noop (Noop recycle, pods survive release)")
			applyTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolNoop,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      6,
				"PoolMin":      3,
				"RecycleType":  "Noop",
				"Command":      `["sleep", "3600"]`,
			}, poolNoop+".yaml")

			By("creating Pool-Delete (Delete recycle, pods are terminated on release)")
			applyTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolDelete,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      5,
				"PoolMin":      2,
				"RecycleType":  "Delete",
				"Command":      `["sleep", "3600"]`,
			}, poolDelete+".yaml")

			By("creating Pool-Restart (Restart recycle, containers are restarted on release)")
			applyTemplate("testdata/pool-with-recycle.yaml", map[string]interface{}{
				"PoolName":     poolRestart,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      6,
				"PoolMin":      3,
				"RecycleType":  "Restart",
			}, poolRestart+".yaml")

			waitPoolStable(poolNoop, testNamespace, 3*time.Minute)
			waitPoolStable(poolDelete, testNamespace, 3*time.Minute)
			waitPoolStable(poolRestart, testNamespace, 3*time.Minute)

			// ── Phase-1: interleaved initial allocation across all three pools ──────
			const (
				bsNoop1    = "test-pai-mixed-noop-bs1"    // replicas=2
				bsDelete1  = "test-pai-mixed-delete-bs1"  // replicas=1
				bsRestart1 = "test-pai-mixed-restart-bs1" // replicas=3
			)
			By("phase-1: allocating BS against each pool concurrently")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsNoop1,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolNoop,
			}, bsNoop1+".yaml")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsDelete1,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolDelete,
			}, bsDelete1+".yaml")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsRestart1,
				"Namespace":        testNamespace,
				"Replicas":         3,
				"PoolName":         poolRestart,
			}, bsRestart1+".yaml")

			By("phase-1: waiting for all BS to be fully allocated")
			Eventually(func(g Gomega) {
				for _, spec := range []struct {
					name string
					want string
				}{
					{bsNoop1, "2"}, {bsDelete1, "1"}, {bsRestart1, "3"},
				} {
					cmd := exec.Command("kubectl", "get", "batchsandbox", spec.name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal(spec.want), "bs %s allocated mismatch", spec.name)
				}
			}, 3*time.Minute).Should(Succeed())

			// Snapshot pod sets and restart counts before any release.
			noopPods1 := getAllocatedPods(bsNoop1, testNamespace, 2)
			deletePods1 := getAllocatedPods(bsDelete1, testNamespace, 1)
			restartPods1 := getAllocatedPods(bsRestart1, testNamespace, 3)

			By("phase-1: verifying cross-pool pod isolation (no pod shared between pools)")
			allPhase1 := make(map[string]string)
			for _, p := range noopPods1 {
				allPhase1[p] = poolNoop
			}
			for _, p := range deletePods1 {
				_, dup := allPhase1[p]
				Expect(dup).To(BeFalse(), "pod %s shared between pool-noop and pool-delete", p)
				allPhase1[p] = poolDelete
			}
			for _, p := range restartPods1 {
				_, dup := allPhase1[p]
				Expect(dup).To(BeFalse(), "pod %s shared between a previous pool and pool-restart", p)
				allPhase1[p] = poolRestart
			}

			// Record restart counts for Restart-pool pods before releasing.
			restartCountsBefore := make(map[string]int32)
			for _, podName := range restartPods1 {
				cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
				out, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				var cnt int32
				fmt.Sscanf(out, "%d", &cnt)
				restartCountsBefore[podName] = cnt
			}

			// ── Phase-2: release pool-Noop and pool-Delete BS concurrently ──────────
			By("phase-2: releasing bsNoop1 (Noop) and bsDelete1 (Delete) concurrently, while bsRestart1 is still held")
			cmd := exec.Command("kubectl", "delete", "batchsandbox", bsNoop1, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsDelete1, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)

			By("phase-2: pool-Noop allocated must drop to 0")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolNoop, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("0"), "pool-noop allocated should be 0 after bsNoop1 released")
			}, 2*time.Minute).Should(Succeed())

			By("phase-2: pool-Delete allocated must drop to 0")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolDelete, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("0"), "pool-delete allocated should be 0 after bsDelete1 released")
			}, 2*time.Minute).Should(Succeed())

			By("phase-2: Noop pods must still be alive (not deleted)")
			Consistently(func(g Gomega) {
				for _, pod := range noopPods1 {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "Noop pod %s should still exist", pod)
					g.Expect(out).To(BeEmpty(), "Noop pod %s must not be terminating", pod)
				}
			}, 15*time.Second, 2*time.Second).Should(Succeed())

			By("phase-2: Delete pods must be terminating or already gone")
			Eventually(func(g Gomega) {
				for _, pod := range deletePods1 {
					cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
						"-o", "jsonpath={.metadata.deletionTimestamp}")
					out, err := utils.Run(cmd)
					if err != nil && strings.Contains(err.Error(), "not found") {
						continue
					}
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).NotTo(BeEmpty(), "Delete pod %s should be terminating", pod)
				}
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("phase-2: pool-Restart allocated must still be 3 (bsRestart1 not yet released)")
			cmd = exec.Command("kubectl", "get", "pool", poolRestart, "-n", testNamespace,
				"-o", "jsonpath={.status.allocated}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("3"), "pool-restart allocated must remain 3 while bsRestart1 is held")

			// ── Phase-3: second-wave alloc on Noop/Delete + release Restart BS ──────
			const (
				bsNoop2   = "test-pai-mixed-noop-bs2"   // replicas=1
				bsDelete2 = "test-pai-mixed-delete-bs2" // replicas=2
			)
			By("phase-3: allocating second-wave BS on pool-Noop(1) and pool-Delete(2)")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsNoop2,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"PoolName":         poolNoop,
			}, bsNoop2+".yaml")
			applyTemplate("testdata/batchsandbox-pooled-no-expire.yaml", map[string]interface{}{
				"BatchSandboxName": bsDelete2,
				"Namespace":        testNamespace,
				"Replicas":         2,
				"PoolName":         poolDelete,
			}, bsDelete2+".yaml")

			By("phase-3: releasing bsRestart1 concurrently with the second-wave allocation")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", bsRestart1, "-n", testNamespace, "--wait=false")
			_, _ = utils.Run(cmd)

			By("phase-3: waiting for second-wave BS to be allocated")
			Eventually(func(g Gomega) {
				for _, spec := range []struct {
					name string
					want string
				}{
					{bsNoop2, "1"}, {bsDelete2, "2"},
				} {
					cmd := exec.Command("kubectl", "get", "batchsandbox", spec.name, "-n", testNamespace,
						"-o", "jsonpath={.status.allocated}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal(spec.want), "bs %s allocated mismatch", spec.name)
				}
			}, 3*time.Minute).Should(Succeed())

			By("phase-3: Restart-pool pods must get container-restarted after bsRestart1 is released")
			Eventually(func(g Gomega) {
				for _, podName := range restartPods1 {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
					out, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					var cnt int32
					fmt.Sscanf(out, "%d", &cnt)
					g.Expect(cnt).To(BeNumerically(">", restartCountsBefore[podName]),
						"pod %s restartCount should increase after Restart recycle", podName)
				}
			}, 2*time.Minute).Should(Succeed())

			By("phase-3: pool-Restart allocated must drop to 0 after bsRestart1 released")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolRestart, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("0"), "pool-restart allocated should be 0 after release")
			}, 2*time.Minute).Should(Succeed())

			// ── Phase-4: final cross-pool invariant checks ───────────────────────────
			// Noop semantic: phase-1 pods must still be alive (not deleted), even after BS release.
			// The pool may or may not reuse the exact same pod for bsNoop2 (depends on scheduling
			// order), so we verify pod survival rather than specific pod reuse.
			By("phase-4: verifying phase-1 Noop pods are still alive (not deleted by Noop recycle)")
			for _, pod := range noopPods1 {
				cmd := exec.Command("kubectl", "get", "pod", pod, "-n", testNamespace,
					"-o", "jsonpath={.metadata.deletionTimestamp}")
				out, err := utils.Run(cmd)
				if err == nil {
					// Pod exists: must not be terminating
					Expect(out).To(BeEmpty(), "Noop pod %s from phase-1 must not be terminating", pod)
				}
				// If pod was evicted by pool capacity management that's OK; Noop just means
				// the recycle handler itself does not delete it.
			}

			By("phase-4: verifying second-wave bsNoop2 has a valid allocated pod")
			noopPods2 := getAllocatedPods(bsNoop2, testNamespace, 1)
			Expect(noopPods2).To(HaveLen(1), "bsNoop2 must have exactly 1 allocated pod")

			By("phase-4: verifying second-wave pool-Delete pods are NOT the same as phase-1 Delete pods (were deleted)")
			deletePods2 := getAllocatedPods(bsDelete2, testNamespace, 2)
			deletePods1Set := make(map[string]bool)
			for _, p := range deletePods1 {
				deletePods1Set[p] = true
			}
			for _, p := range deletePods2 {
				Expect(deletePods1Set).NotTo(HaveKey(p),
					"Delete pod %s should not be reused (it was deleted during recycle)", p)
			}

			By("phase-4: verifying all three pools' allocated counts are still correct")
			for _, spec := range []struct {
				pool string
				want string
			}{
				{poolNoop, "1"},    // bsNoop2 holds 1
				{poolDelete, "2"},  // bsDelete2 holds 2
				{poolRestart, "0"}, // all released
			} {
				cmd := exec.Command("kubectl", "get", "pool", spec.pool, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				out, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal(spec.want), "pool %s allocated mismatch in final check", spec.pool)
			}

			By("phase-4: verifying pool-Restart has recovered available pods for new allocations")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolRestart, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				avail := 0
				fmt.Sscanf(out, "%d", &avail)
				g.Expect(avail).To(BeNumerically(">=", 1),
					"pool-restart should have available pods after container restart recycle")
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			for _, bs := range []string{bsNoop2, bsDelete2} {
				cmd := exec.Command("kubectl", "delete", "batchsandbox", bs, "-n", testNamespace, "--wait=false")
				_, _ = utils.Run(cmd)
			}
			for _, pool := range []string{poolNoop, poolDelete, poolRestart} {
				cmd := exec.Command("kubectl", "delete", "pool", pool, "-n", testNamespace)
				_, _ = utils.Run(cmd)
			}
		})
	})

	Context("Pool Auto-Assign", Label("Pool"), func() {
		BeforeAll(func() {
			By("waiting for controller to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})

		It("should auto-assign BatchSandbox without template to the only Pool when poolRef is *", func() {
			const poolName = "test-pool-auto-assign"
			const testNamespace = "default"

			By("creating a Pool")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    4,
				"BufferMin":    2,
				"PoolMax":      6,
				"PoolMin":      3,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", poolName+".yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 3))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox without template and poolRef: *")
			const batchSandboxName = "test-bs-auto-assign"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         2,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated from * to the actual Pool name")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolName))
			}, 2*time.Minute).Should(Succeed())

			By("verifying pods are allocated to the BatchSandbox")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("2"))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the image-matching Pool among multiple Pools", func() {
			const poolMatchName = "test-pool-match"
			const poolOtherName = "test-pool-other"
			const testNamespace = "default"

			By("creating pool-match with the sandbox image")
			poolMatchYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolMatchName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolMatchFile := filepath.Join("/tmp", poolMatchName+".yaml")
			err = os.WriteFile(poolMatchFile, []byte(poolMatchYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolMatchFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolMatchFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-other with a different image")
			poolOtherYAML := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: busybox:1.36
        command: ["sleep", "3600"]
  capacitySpec:
    bufferMax: 3
    bufferMin: 2
    poolMax: 5
    poolMin: 2`, poolOtherName, testNamespace)
			poolOtherFile := filepath.Join("/tmp", poolOtherName+".yaml")
			err = os.WriteFile(poolOtherFile, []byte(poolOtherYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolOtherFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolOtherFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-match pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolMatchName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with template specifying the sandbox image")
			const batchSandboxName = "test-bs-image-match"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the image-matching Pool")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolMatchName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolMatchName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolOtherName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the nodeSelector-matching Pool among multiple Pools", func() {
			const poolSSDName = "test-pool-ssd"
			const poolHDDName = "test-pool-hdd"
			const testNamespace = "default"

			By("labeling the Kind node with disk=ssd for nodeSelector scheduling")
			cmd := exec.Command("kubectl", "label", "node", "sandbox-k8s-test-e2e-control-plane", "disk=ssd", "--overwrite")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "label", "node", "sandbox-k8s-test-e2e-control-plane", "disk-")
				_, _ = utils.Run(cmd)
			}()

			By("creating pool-ssd with nodeSelector disk=ssd")
			poolSSDYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolSSDName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"NodeSelector": map[string]interface{}{"disk": "ssd"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolSSDFile := filepath.Join("/tmp", poolSSDName+".yaml")
			err = os.WriteFile(poolSSDFile, []byte(poolSSDYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolSSDFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolSSDFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-hdd with nodeSelector disk=hdd")
			poolHDDYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolHDDName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"NodeSelector": map[string]interface{}{"disk": "hdd"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolHDDFile := filepath.Join("/tmp", poolHDDName+".yaml")
			err = os.WriteFile(poolHDDFile, []byte(poolHDDYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolHDDFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolHDDFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-ssd pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolSSDName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with template specifying nodeSelector disk=ssd")
			const batchSandboxName = "test-bs-nodeselector"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
				"NodeSelector":     map[string]interface{}{"disk": "ssd"},
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the nodeSelector-matching Pool")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolSSDName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolSSDName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolHDDName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the Pool whose labels match BatchSandbox nodeAffinity", func() {
			const poolGPUName = "test-pool-gpu"
			const poolCPUName = "test-pool-cpu"
			const testNamespace = "default"

			By("creating pool-gpu with label accelerator=gpu")
			poolGPUYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolGPUName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"Labels":       map[string]interface{}{"accelerator": "gpu"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolGPUFile := filepath.Join("/tmp", poolGPUName+".yaml")
			err = os.WriteFile(poolGPUFile, []byte(poolGPUYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolGPUFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolGPUFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-cpu with label accelerator=cpu")
			poolCPUYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolCPUName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"Labels":       map[string]interface{}{"accelerator": "cpu"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolCPUFile := filepath.Join("/tmp", poolCPUName+".yaml")
			err = os.WriteFile(poolCPUFile, []byte(poolCPUYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolCPUFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolCPUFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-gpu pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolGPUName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with nodeAffinity requiring accelerator=gpu")
			const batchSandboxName = "test-bs-affinity"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
				"AffinityYAML":     "\n        nodeAffinity:\n          requiredDuringSchedulingIgnoredDuringExecution:\n            nodeSelectorTerms:\n            - matchExpressions:\n              - key: accelerator\n                operator: In\n                values:\n                - gpu",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the Pool with matching labels")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolGPUName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolGPUName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolCPUName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the Pool whose labels match BatchSandbox nodeSelector", func() {
			const poolGPUName = "test-pool-label-gpu"
			const poolCPUName = "test-pool-label-cpu"
			const testNamespace = "default"

			By("creating pool-label-gpu with label accelerator=gpu")
			poolGPUYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolGPUName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"Labels":       map[string]interface{}{"accelerator": "gpu"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolGPUFile := filepath.Join("/tmp", poolGPUName+".yaml")
			err = os.WriteFile(poolGPUFile, []byte(poolGPUYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolGPUFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolGPUFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-label-cpu with label accelerator=cpu")
			poolCPUYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolCPUName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"Labels":       map[string]interface{}{"accelerator": "cpu"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolCPUFile := filepath.Join("/tmp", poolCPUName+".yaml")
			err = os.WriteFile(poolCPUFile, []byte(poolCPUYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolCPUFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolCPUFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-label-gpu pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolGPUName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with nodeSelector accelerator=gpu")
			const batchSandboxName = "test-bs-nodeselector-labels"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
				"NodeSelector":     map[string]interface{}{"accelerator": "gpu"},
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the Pool with matching labels")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolGPUName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolGPUName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolCPUName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the Pool whose nodeSelector matches BatchSandbox nodeAffinity", func() {
			const poolSSDName = "test-pool-ns-ssd"
			const poolHDDName = "test-pool-ns-hdd"
			const testNamespace = "default"

			By("labeling the Kind node with disk=ssd for nodeSelector scheduling")
			cmd := exec.Command("kubectl", "label", "node", "sandbox-k8s-test-e2e-control-plane", "disk=ssd", "--overwrite")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "label", "node", "sandbox-k8s-test-e2e-control-plane", "disk-")
				_, _ = utils.Run(cmd)
			}()

			By("creating pool-ns-ssd with nodeSelector disk=ssd")
			poolSSDYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolSSDName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"NodeSelector": map[string]interface{}{"disk": "ssd"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolSSDFile := filepath.Join("/tmp", poolSSDName+".yaml")
			err = os.WriteFile(poolSSDFile, []byte(poolSSDYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolSSDFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolSSDFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-ns-hdd with nodeSelector disk=hdd")
			poolHDDYAML, err := renderTemplate("testdata/pool-with-nodeselector.yaml", map[string]interface{}{
				"PoolName":     poolHDDName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    2,
				"PoolMax":      5,
				"PoolMin":      2,
				"NodeSelector": map[string]interface{}{"disk": "hdd"},
			})
			Expect(err).NotTo(HaveOccurred())

			poolHDDFile := filepath.Join("/tmp", poolHDDName+".yaml")
			err = os.WriteFile(poolHDDFile, []byte(poolHDDYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolHDDFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolHDDFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-ns-ssd pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolSSDName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox with nodeAffinity requiring disk=ssd")
			const batchSandboxName = "test-bs-affinity-nodeselector"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
				"AffinityYAML":     "\n        nodeAffinity:\n          requiredDuringSchedulingIgnoredDuringExecution:\n            nodeSelectorTerms:\n            - matchExpressions:\n              - key: disk\n                operator: In\n                values:\n                - ssd",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the Pool with matching nodeSelector")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolSSDName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolSSDName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolHDDName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should auto-assign to the Pool satisfying resource requests", func() {
			const poolLargeName = "test-pool-large"
			const poolSmallName = "test-pool-small"
			const testNamespace = "default"

			By("creating pool-large with container CPU request 500m")
			poolLargeYAML := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: %s
        command: ["sleep", "3600"]
        resources:
          requests:
            cpu: "500m"
            memory: "512Mi"
  capacitySpec:
    bufferMax: 3
    bufferMin: 2
    poolMax: 5
    poolMin: 2`, poolLargeName, testNamespace, utils.SandboxImage)
			poolLargeFile := filepath.Join("/tmp", poolLargeName+".yaml")
			err := os.WriteFile(poolLargeFile, []byte(poolLargeYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolLargeFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolLargeFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating pool-small with container CPU request 50m")
			poolSmallYAML := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: %s
        command: ["sleep", "3600"]
        resources:
          requests:
            cpu: "50m"
            memory: "32Mi"
  capacitySpec:
    bufferMax: 3
    bufferMin: 2
    poolMax: 5
    poolMin: 2`, poolSmallName, testNamespace, utils.SandboxImage)
			poolSmallFile := filepath.Join("/tmp", poolSmallName+".yaml")
			err = os.WriteFile(poolSmallFile, []byte(poolSmallYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolSmallFile)

			cmd = exec.Command("kubectl", "apply", "-f", poolSmallFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pool-large pods to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolLargeName),
					"--field-selector=status.phase=Running",
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				pods := strings.Fields(output)
				g.Expect(len(pods)).To(BeNumerically(">=", 2))
			}, 3*time.Minute).Should(Succeed())

			By("creating a BatchSandbox requesting CPU 200m which pool-small cannot satisfy")
			const batchSandboxName = "test-bs-resource"
			bsYAML, err := renderTemplate("testdata/batchsandbox-auto-assign-with-template.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"Namespace":        testNamespace,
				"Replicas":         1,
				"SandboxImage":     utils.SandboxImage,
				"Resources":        map[string]interface{}{"cpu": "200m", "memory": "128Mi"},
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", batchSandboxName+".yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying poolRef was updated to the resource-sufficient Pool")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.spec.poolRef}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(poolLargeName))
			}, 2*time.Minute).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolLargeName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolSmallName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

})

// waitPoolStable waits until pool.status.available + pool.status.allocated == pool.status.total,
// ensuring all pool pods are either ready or allocated before proceeding.
func waitPoolStable(poolName, testNamespace string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
			"-o", "jsonpath={.status.total}")
		totalStr, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		total := 0
		fmt.Sscanf(totalStr, "%d", &total)
		g.Expect(total).To(BeNumerically(">", 0), "pool total should be > 0")

		cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
			"-o", "jsonpath={.status.available}")
		availableStr, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		available := 0
		fmt.Sscanf(availableStr, "%d", &available)

		cmd = exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
			"-o", "jsonpath={.status.allocated}")
		allocatedStr, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		allocated := 0
		fmt.Sscanf(allocatedStr, "%d", &allocated)

		g.Expect(available+allocated).To(Equal(total), "pool available+allocated should equal total")
	}, timeout).Should(Succeed())
}

// getAllocatedPods reads the alloc-status annotation of a BatchSandbox and returns the pod names.
func getAllocatedPods(bsName, testNamespace string, expectedLen int) []string {
	var pods []string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "batchsandbox", bsName, "-n", testNamespace,
			"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty())
		var alloc struct {
			Pods []string `json:"pods"`
		}
		g.Expect(json.Unmarshal([]byte(out), &alloc)).To(Succeed())
		g.Expect(alloc.Pods).To(HaveLen(expectedLen))
		pods = alloc.Pods
	}, 2*time.Minute).Should(Succeed())
	return pods
}

// applyTemplate renders and applies a template, returning a cleanup func.
func applyTemplate(templateFile string, data map[string]interface{}, tmpFile string) {
	yaml, err := renderTemplate(templateFile, data)
	Expect(err).NotTo(HaveOccurred())
	f := filepath.Join("/tmp", tmpFile)
	Expect(os.WriteFile(f, []byte(yaml), 0644)).To(Succeed())
	cmd := exec.Command("kubectl", "apply", "-f", f)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// renderTemplate renders a YAML template file with the given data.
func renderTemplate(templateFile string, data map[string]interface{}) (string, error) {
	dir, err := utils.GetProjectDir()
	if err != nil {
		return "", err
	}

	fullPath := filepath.Join(dir, "test", "e2e", templateFile)
	tmplContent, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to read template file %s: %w", fullPath, err)
	}

	tmpl, err := template.New("yaml").Parse(string(tmplContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// restartController scales down the controller-manager deployment to 0 and back to 1,
// simulating a controller restart to test state reconstruction.
func restartController() error {
	By("scaling down controller-manager to simulate restart")
	cmd := exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
		"--replicas=0", "-n", namespace)
	_, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to scale down controller: %w", err)
	}

	By("waiting for controller pod to terminate")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
		output, err := utils.Run(cmd)
		// Pod list should be empty when all pods are terminated
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(BeEmpty())
	}, 30*time.Second, 2*time.Second).Should(Succeed())

	By("scaling up controller-manager")
	cmd = exec.Command("kubectl", "scale", "deployment", "opensandbox-controller-manager",
		"--replicas=1", "-n", namespace)
	_, err = utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to scale up controller: %w", err)
	}

	By("waiting for controller to be ready after restart")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Running"))
	}, 2*time.Minute, 5*time.Second).Should(Succeed())

	return nil
}
