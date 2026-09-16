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
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

func testPoolRefGuard() {
	const sandboxName = "poolref-guard"
	const poolA = "poolref-guard-a"
	const poolB = "poolref-guard-b"
	kubectl := func(args ...string) (string, error) { return utils.Run(exec.Command("kubectl", args...)) }
	DeferCleanup(func() {
		_, _ = kubectl("delete", "batchsandbox", sandboxName, "--ignore-not-found", "--timeout=60s")
		_, _ = kubectl("delete", "pool", poolA, poolB, "--ignore-not-found", "--timeout=60s")
	})
	By("creating two pools with the default destructive recycler")
	for _, name := range []string{poolA, poolB} {
		manifest := fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
      - name: main
        image: registry.k8s.io/pause:3.6
  capacitySpec:
    poolMin: 0
    poolMax: 3
    bufferMin: 1
    bufferMax: 1
`, name)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: %s
spec:
  replicas: 1
  poolRef: %s
`, sandboxName, poolA))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	var allocatedPod string
	readSandbox := func(g Gomega) *sandboxv1alpha1.BatchSandbox {
		output, err := kubectl("get", "batchsandbox", sandboxName, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())
		sandbox := &sandboxv1alpha1.BatchSandbox{}
		g.Expect(json.Unmarshal([]byte(output), sandbox)).To(Succeed())
		return sandbox
	}
	Eventually(func(g Gomega) {
		sandbox := readSandbox(g)
		g.Expect(sandbox.Status.Ready).To(Equal(int32(1)))
		var allocation struct {
			Pods []string `json:"pods"`
		}
		g.Expect(json.Unmarshal([]byte(sandbox.Annotations["sandbox.opensandbox.io/alloc-status"]), &allocation)).To(Succeed())
		g.Expect(allocation.Pods).To(HaveLen(1))
		allocatedPod = allocation.Pods[0]
	}, 2*time.Minute, time.Second).Should(Succeed())

	for _, target := range []string{poolB, "*"} {
		By("requesting the unsupported pool reference " + target)
		_, err := kubectl("patch", "batchsandbox", sandboxName, "--type=merge", "-p", fmt.Sprintf(`{"spec":{"poolRef":%q}}`, target))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			sandbox := readSandbox(g)
			g.Expect(sandbox.Status.Conditions).To(ContainElement(And(
				HaveField("Type", sandboxv1alpha1.BatchSandboxConditionPoolRefUpdateRejected),
				HaveField("Message", ContainSubstring(target)),
			)))
		}, time.Minute, time.Second).Should(Succeed())
		Consistently(func(g Gomega) {
			sandbox := readSandbox(g)
			g.Expect(sandbox.Spec.PoolRef).To(Equal(target))
			g.Expect(sandbox.Status.Ready).To(Equal(int32(1)))
			var allocation struct {
				Pods    []string `json:"pods"`
				PoolRef string   `json:"poolRef"`
			}
			g.Expect(json.Unmarshal([]byte(sandbox.Annotations["sandbox.opensandbox.io/alloc-status"]), &allocation)).To(Succeed())
			g.Expect(allocation.Pods).To(ConsistOf(allocatedPod))
			g.Expect(allocation.PoolRef).To(Equal(poolA))
			output, err := kubectl("get", "pod", allocatedPod, "-o", "json")
			g.Expect(err).NotTo(HaveOccurred())
			pod := &corev1.Pod{}
			g.Expect(json.Unmarshal([]byte(output), pod)).To(Succeed())
			g.Expect(pod.DeletionTimestamp).To(BeNil())
		}, 10*time.Second, time.Second).Should(Succeed())
	}
}
