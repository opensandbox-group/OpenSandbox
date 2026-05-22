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
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting sandbox-k8s integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if utils.SkipImageBuild() {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"E2E_MODE=%s SKIP_IMAGE_BUILD=true: skipping docker build & kind load (images expected to be pre-built)\n",
			utils.Mode())
		return
	}

	dockerBuildArgs := os.Getenv("DOCKER_BUILD_ARGS")

	By("building the manager(Operator) image")
	makeArgs := []string{"docker-build", fmt.Sprintf("CONTROLLER_IMG=%s", utils.ControllerImage)}
	if dockerBuildArgs != "" {
		makeArgs = append(makeArgs, fmt.Sprintf("DOCKER_BUILD_ARGS=%s", dockerBuildArgs))
	}
	cmd := exec.Command("make", makeArgs...)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	By("building the task-executor image")
	makeArgs = []string{"docker-build-task-executor", fmt.Sprintf("TASK_EXECUTOR_IMG=%s", utils.TaskExecutorImage)}
	if dockerBuildArgs != "" {
		makeArgs = append(makeArgs, fmt.Sprintf("DOCKER_BUILD_ARGS=%s", dockerBuildArgs))
	}
	cmd = exec.Command("make", makeArgs...)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the task-executor image")

	By("building the image-committer image")
	makeArgs = []string{"docker-build-image-committer", fmt.Sprintf("IMAGE_COMMITTER_IMG=%s", utils.ImageCommitterImage)}
	if dockerBuildArgs != "" {
		makeArgs = append(makeArgs, fmt.Sprintf("DOCKER_BUILD_ARGS=%s", dockerBuildArgs))
	}
	cmd = exec.Command("make", makeArgs...)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the image-committer image")

	// If you want to change the e2e test vendor from Kind, ensure the image is
	// built and available before running the tests. Also, remove the following block.
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(utils.ControllerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	By("loading the task-executor image on Kind")
	err = utils.LoadImageToKindClusterWithName(utils.TaskExecutorImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the task-executor image into Kind")

	By("loading the image-committer image on Kind")
	err = utils.LoadImageToKindClusterWithName(utils.ImageCommitterImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the image-committer image into Kind")

	By("pulling the registry image (required for pause/resume tests)")
	cmd = exec.Command("docker", "pull", "--platform", "linux/amd64", utils.RegistrySourceImage())
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull registry image")

	By("loading the registry image on Kind")
	err = utils.LoadImageToKindClusterWithName(utils.RegistrySourceImage())
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the registry image into Kind")

	By("pulling the alpine image (required for commit jobs)")
	cmd = exec.Command("docker", "pull", utils.AlpineImage())
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull alpine image")

	By("loading the alpine image on Kind")
	err = utils.LoadImageToKindClusterWithName(utils.AlpineImage())
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the alpine image into Kind")
})

var _ = AfterSuite(func() {
})
