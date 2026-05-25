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

package gvisor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// runKubectl executes a kubectl command from the project root directory
func runKubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = "../../.." // Navigate from test/e2e_runtime/gvisor to project root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("kubectl %v failed: %w", args, err)
	}
	return string(output), nil
}

var _ = Describe("gVisor RuntimeClass", Ordered, func() {
	const testNamespace = "default"

	BeforeAll(func() {
		By("installing gVisor RuntimeClass")
		_, err := runKubectl("apply", "-f", "test/e2e_runtime/gvisor/testdata/runtimeclass.yaml")
		Expect(err).NotTo(HaveOccurred(), "Failed to create gVisor RuntimeClass")
	})

	AfterAll(func() {
		By("cleaning up RuntimeClass")
		_, _ = runKubectl("delete", "runtimeclass", RuntimeClassName, "--ignore-not-found=true")
	})

	Context("RuntimeClass API", func() {
		It("should create RuntimeClass resources", func() {
			By("verifying RuntimeClass exists")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "runtimeclass", RuntimeClassName, "-o", "json")
				g.Expect(err).NotTo(HaveOccurred())

				var rcObj struct {
					Handler string `json:"handler"`
				}
				err = json.Unmarshal([]byte(output), &rcObj)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rcObj.Handler).To(Equal("runsc"))
			}, 30*time.Second).Should(Succeed())
		})
	})

	Context("Pod with runtimeClassName", func() {
		var podName string

		BeforeEach(func() {
			podName = fmt.Sprintf("test-pod-gvisor-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			By("cleaning up Pod")
			if podName != "" {
				_, _ = runKubectl("delete", "pod", podName, "-n", testNamespace, "--ignore-not-found=true")
			}
		})

		It("should create Pod with runtimeClassName", func() {
			By("creating a Pod with runtimeClassName")
			podYAML := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  runtimeClassName: %s
  containers:
  - name: test-container
    image: %s
    command: ["sleep", "3600"]
`, podName, testNamespace, RuntimeClassName, utils.SandboxImage)

			podFile := filepath.Join("/tmp", fmt.Sprintf("test-pod-%s.yaml", podName))
			err := os.WriteFile(podFile, []byte(podYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(podFile)

			_, err = runKubectl("apply", "-f", podFile)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Pod")

			By("verifying Pod has runtimeClassName set")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.spec.runtimeClassName}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(RuntimeClassName))
			}, 30*time.Second).Should(Succeed())

			By("verifying Pod is running with gVisor")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())
		})
	})

	Context("Pool with gVisor RuntimeClass", func() {
		var poolName string
		var batchSandboxName string

		BeforeEach(func() {
			poolName = fmt.Sprintf("gvisor-pool-%d", time.Now().UnixNano())
			batchSandboxName = fmt.Sprintf("gvisor-bsbx-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			By("cleaning up BatchSandbox")
			if batchSandboxName != "" {
				_, _ = runKubectl("delete", "batchsandbox", batchSandboxName, "-n", testNamespace, "--ignore-not-found=true")
			}
			By("cleaning up Pool")
			if poolName != "" {
				_, _ = runKubectl("delete", "pool", poolName, "-n", testNamespace, "--ignore-not-found=true")
			}
		})

		It("should create Pool and allocate Pod with gVisor runtime", func() {
			By("creating a Pool with gVisor runtimeClassName")
			poolYAML := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      runtimeClassName: %s
      containers:
        - name: sandbox-container
          image: %s
          command: ["sleep", "3600"]
  capacitySpec:
    bufferMax: 2
    bufferMin: 1
    poolMax: 5
    poolMin: 1
`, poolName, testNamespace, RuntimeClassName, utils.SandboxImage)

			poolFile := filepath.Join("/tmp", fmt.Sprintf("test-pool-%s.yaml", poolName))
			err := os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			_, err = runKubectl("apply", "-f", poolFile)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Pool")

			By("waiting for Pool to have available pods")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pool", poolName, "-n", testNamespace, "-o", "json")
				g.Expect(err).NotTo(HaveOccurred())

				var poolObj struct {
					Status struct {
						Available int32 `json:"available"`
					} `json:"status"`
				}
				err = json.Unmarshal([]byte(output), &poolObj)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(poolObj.Status.Available).To(BeNumerically(">", 0))
			}, 3*time.Minute).Should(Succeed())

			By("creating BatchSandbox with poolRef")
			bsbxYAML := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  poolRef: %s
`, batchSandboxName, testNamespace, poolName)

			bsbxFile := filepath.Join("/tmp", fmt.Sprintf("test-bsbx-%s.yaml", batchSandboxName))
			err = os.WriteFile(bsbxFile, []byte(bsbxYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsbxFile)

			_, err = runKubectl("apply", "-f", bsbxFile)
			Expect(err).NotTo(HaveOccurred(), "Failed to create BatchSandbox")

			By("waiting for BatchSandbox to allocate a Pod")
			var podName string
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[0].metadata.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				podName = output
			}, 2*time.Minute).Should(Succeed())

			By("verifying allocated Pod has runtimeClassName set to gVisor")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.spec.runtimeClassName}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(RuntimeClassName))
			}, 30*time.Second).Should(Succeed())

			By("verifying Pod is running with gVisor")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying BatchSandbox status is ready")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.ready}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())
		})
	})
})
