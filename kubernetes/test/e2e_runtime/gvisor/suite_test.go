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
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// RuntimeClassName is the name of the RuntimeClass for gVisor
	RuntimeClassName = "gvisor"
)

// KindCluster is the name of the Kind cluster for gVisor tests.
// It reads from KIND_CLUSTER environment variable, defaulting to "gvisor-test".
var KindCluster = getKindCluster()

func getKindCluster() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		return v
	}
	return "gvisor-test"
}

// TestGVisorRuntimeClass runs the gVisor RuntimeClass end-to-end tests.
// These tests validate gVisor functionality with the Kind cluster
// configured specifically for gVisor (runsc) runtime.
func TestGVisorRuntimeClass(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gVisor RuntimeClass E2E test suite\n")
	RunSpecs(t, "gVisor runtimeclass suite")
}

var _ = BeforeSuite(func() {
	dockerBuildArgs := os.Getenv("DOCKER_BUILD_ARGS")

	By("building task-executor image")
	makeArgs := []string{"docker-build-task-executor", fmt.Sprintf("TASK_EXECUTOR_IMG=%s", utils.TaskExecutorImage)}
	if dockerBuildArgs != "" {
		makeArgs = append(makeArgs, fmt.Sprintf("DOCKER_BUILD_ARGS=%s", dockerBuildArgs))
	}
	cmd := exec.Command("make", makeArgs...)
	cmd.Dir = "../../.." // Navigate from test/e2e_runtime/gvisor to project root
	output, err := cmd.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build task-executor image: %s", string(output))

	By("loading task-executor image on Kind")
	// Use kind command directly to load image, avoiding utils.GetProjectDir() path issues
	cmd = exec.Command("kind", "load", "docker-image", "--name", KindCluster, utils.TaskExecutorImage)
	cmd.Dir = "../../.." // Navigate from test/e2e_runtime/gvisor to project root
	output, err = cmd.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load task-executor image into Kind: %s", string(output))
})

var _ = AfterSuite(func() {
})
