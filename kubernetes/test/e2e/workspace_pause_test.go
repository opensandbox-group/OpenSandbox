// Copyright 2025 The OpenSandbox Authors
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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

// Runs in the PauseResume suite's disposable namespace. Supply a CSI-backed claim
// via E2E_WORKSPACE_PVC to exercise the same flow against object storage.
func testWorkspacePause() {
	name := "workspace-pause"
	pod := name + "-0"
	kubectl := func(args ...string) (string, error) {
		return utils.Run(exec.Command("kubectl", append([]string{"-n", pauseResumeNamespace}, args...)...))
	}
	create := func(manifest string) {
		cmd := exec.Command("kubectl", "-n", pauseResumeNamespace, "create", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}
	claim := os.Getenv("E2E_WORKSPACE_PVC")
	if claim == "" {
		claim = name
		create(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-pause
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 64Mi}
`)
		DeferCleanup(func() {
			_, err := kubectl("delete", "pvc", claim, "--ignore-not-found")
			Expect(err).NotTo(HaveOccurred())
		})
	}
	create(fmt.Sprintf(`apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: %s
spec:
  replicas: 1
  workspacePause: {containerName: sandbox, volumeName: bucket}
  template:
    spec:
      containers:
        - name: sandbox
          image: python:3.12-alpine
          command: ["sleep", "3600"]
          volumeMounts: [{name: bucket, mountPath: /mnt/opensandbox-checkpoints}]
      volumes:
        - name: bucket
          persistentVolumeClaim: {claimName: %s}
`, name, claim))
	DeferCleanup(func() {
		_, err := kubectl("delete", "batchsandbox", name, "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred())
	})
	phase := func(p string) {
		Eventually(func() (string, error) { return kubectl("get", "batchsandbox", name, "-o", "jsonpath={.status.phase}") }, 3*time.Minute, time.Second).Should(Equal(p))
	}
	phase("Succeed")
	sandboxUID, err := kubectl("get", "batchsandbox", name, "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectl("exec", pod, "-c", "sandbox", "--", "sh", "-ec", "mkdir -p /workspace && chown 10001:10002 /workspace && chmod 0750 /workspace")
	Expect(err).NotTo(HaveOccurred())
	asWorkspaceUser := func(script string, args ...string) (string, error) {
		command := []string{"exec", pod, "-c", "sandbox", "--", "python3", "-c",
			"import os, sys\nfrom pathlib import Path\nos.setgroups([])\nos.setgid(10002)\nos.setuid(10001)\n" + script}
		return kubectl(append(command, args...)...)
	}
	_, err = asWorkspaceUser("import venv\nvenv.EnvBuilder(symlinks=True, with_pip=False).create('/workspace/.venv')")
	Expect(err).NotTo(HaveOccurred())
	for round := range 2 {
		marker := fmt.Sprintf("workspace-%d-%d", round, time.Now().UnixNano())
		_, err := asWorkspaceUser("os.umask(0o077)\nPath('/workspace/.marker').write_text(sys.argv[1] + '\\n')\nPath('/tmp/outside-workspace').touch()\nlink = Path('/workspace/current')\nlink.unlink(missing_ok=True)\nlink.symlink_to('/workspace/.marker')", marker)
		Expect(err).NotTo(HaveOccurred())
		uid, err := kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("patch", "batchsandbox", name, "--type=merge", "-p", `{"spec":{"pause":true}}`)
		Expect(err).NotTo(HaveOccurred())
		phase("Paused")
		checkpoint, err := kubectl("get", "sandboxsnapshot", name+"-pause", "-o", `jsonpath={.status.format}{" "}{.status.workspace.archive}{" "}{.status.workspace.sha256}{" "}{.status.workspace.sourcePodUID}`)
		Expect(err).NotTo(HaveOccurred())
		fields := strings.Fields(checkpoint)
		Expect(fields).To(HaveLen(4))
		Expect(fields[0]).To(Equal("workspace-v1"))
		Expect(fields[1]).To(HavePrefix(sandboxUID + "/"))
		Expect(fields[2]).To(MatchRegexp("^[0-9a-f]{64}$"))
		Expect(fields[3]).To(Equal(uid))
		size, err := kubectl("get", "sandboxsnapshot", name+"-pause", "-o", "jsonpath={.status.workspace.sizeBytes}")
		Expect(err).NotTo(HaveOccurred())
		archiveBytes, err := strconv.ParseInt(size, 10, 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(archiveBytes).To(BeNumerically(">", 0))
		jobs, err := kubectl("get", "jobs", "-l", "sandbox.opensandbox.io/sandbox-snapshot-name="+name+"-pause", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(jobs).To(BeEmpty())
		Eventually(func() (string, error) { return kubectl("get", "pod", pod, "--ignore-not-found", "-o", "name") }).Should(BeEmpty())
		_, err = kubectl("patch", "batchsandbox", name, "--type=merge", "-p", `{"spec":{"pause":false}}`)
		Expect(err).NotTo(HaveOccurred())
		phase("Succeed")
		newUID, err := kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		Expect(newUID).NotTo(Equal(uid))
		storage, err := kubectl("get", "pod", pod, "-o", `jsonpath={.spec.volumes[?(@.name=="opensandbox-workspace-restore")].emptyDir.sizeLimit}{" "}{.spec.initContainers[?(@.name=="opensandbox-workspace-restore")].resources.requests.ephemeral-storage}`)
		Expect(err).NotTo(HaveOccurred())
		quantities := strings.Fields(storage)
		Expect(quantities).To(HaveLen(2))
		for _, value := range quantities {
			q, err := resource.ParseQuantity(value)
			Expect(err).NotTo(HaveOccurred())
			Expect(q.Value()).To(BeNumerically(">=", archiveBytes))
		}
		active, err := kubectl("get", "batchsandbox", name, "-o", `go-template={{index .spec.template.metadata.annotations "sandbox.opensandbox.io/workspace-archive"}} {{index .spec.template.metadata.annotations "sandbox.opensandbox.io/workspace-claim"}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(Equal(fields[1] + " " + claim))
		value, err := asWorkspaceUser("assert not Path('/tmp/outside-workspace').exists()\nassert not Path('/workspace/current').readlink().is_absolute()\nprint(Path('/workspace/current').read_text().strip())")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(value)).To(Equal(marker))
		prefix, err := asWorkspaceUser("import subprocess\nprint(subprocess.check_output(['/workspace/.venv/bin/python', '-c', 'import sys; print(sys.prefix)'], text=True).strip())")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(prefix)).To(Equal("/workspace/.venv"))
		fmt.Fprintf(GinkgoWriter, "workspace checkpoint round=%d archive=%s sha256=%s source=%s resumed=%s marker=%s\n",
			round+1, fields[1], fields[2], uid, newUID, marker)
	}
}
