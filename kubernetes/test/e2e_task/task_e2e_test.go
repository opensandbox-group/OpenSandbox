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

package e2e_task

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

const (
	ImageName         = "task-executor-e2e"
	TargetContainer   = "task-e2e-target"
	ExecutorContainer = "task-e2e-executor"
	VolumeName        = "task-e2e-vol"
	HostPort          = "5758"
)

var _ = Describe("Task Executor E2E", Ordered, func() {
	var client *api.Client

	BeforeAll(func() {
		// Check docker
		_, err := exec.LookPath("docker")
		Expect(err).NotTo(HaveOccurred(), "Docker not found, skipping E2E test")

		By("Building image")
		cmd := exec.Command("docker", "build",
			"--build-arg", "PACKAGE=cmd/task-executor/main.go",
			"-t", ImageName, "-f", "../../Dockerfile", "../../")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		Expect(cmd.Run()).To(Succeed())

		By("Cleaning up previous runs")
		exec.Command("docker", "rm", "-f", TargetContainer, ExecutorContainer).Run()
		exec.Command("docker", "volume", "rm", VolumeName).Run()

		By("Creating shared volume")
		Expect(exec.Command("docker", "volume", "create", VolumeName).Run()).To(Succeed())

		By("Starting target container")
		targetCmd := exec.Command("docker", "run", "-d", "--name", TargetContainer,
			"-v", fmt.Sprintf("%s:/tmp/tasks", VolumeName),
			"-e", "SANDBOX_MAIN_CONTAINER=main",
			"-e", "TARGET_VAR=hello-from-target",
			"golang:1.24", "sleep", "infinity")
		targetCmd.Stdout = os.Stdout
		targetCmd.Stderr = os.Stderr
		Expect(targetCmd.Run()).To(Succeed())

		By("Starting executor container in Sidecar Mode")
		execCmd := exec.Command("docker", "run", "-d", "--name", ExecutorContainer,
			"-v", fmt.Sprintf("%s:/tmp/tasks", VolumeName),
			"--privileged",
			"-u", "0",
			"--pid=container:"+TargetContainer,
			"-p", HostPort+":5758",
			ImageName,
			"-enable-sidecar-mode=true",
			"-main-container-name=main",
			"-data-dir=/tmp/tasks")
		execCmd.Stdout = os.Stdout
		execCmd.Stderr = os.Stderr
		Expect(execCmd.Run()).To(Succeed())

		By("Waiting for executor to be ready")
		client = api.NewClient(fmt.Sprintf("http://127.0.0.1:%s", HostPort))
		Eventually(func() error {
			_, err := client.Get(context.Background())
			return err
		}, 10*time.Second, 500*time.Millisecond).Should(Succeed(), "Executor failed to become ready")
	})

	AfterAll(func() {
		By("Cleaning up containers")
		if CurrentSpecReport().Failed() {
			By("Dumping logs")
			out, _ := exec.Command("docker", "logs", ExecutorContainer).CombinedOutput()
			fmt.Printf("Executor Logs:\n%s\n", string(out))
		}
		exec.Command("docker", "rm", "-f", TargetContainer, ExecutorContainer).Run()
		exec.Command("docker", "volume", "rm", VolumeName).Run()
	})

	Context("When creating a short-lived task", func() {
		taskName := "e2e-test-1"

		It("should run and succeed", func() {
			By("Creating task")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"sleep", "2"},
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to succeed")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.Name).To(Equal(taskName))

				// Verify state
				if got.ProcessStatus != nil && got.ProcessStatus.Terminated != nil {
					g.Expect(got.ProcessStatus.Terminated.ExitCode).To(BeZero())
					g.Expect(got.ProcessStatus.Terminated.Reason).To(Equal("Succeeded"))
				} else {
					// Fail if not terminated yet (so Eventually retries)
					g.Expect(got.ProcessStatus).NotTo(BeNil(), "Task ProcessStatus is nil")
					g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				}
			}, 10*time.Second, 1*time.Second).Should(Succeed())
		})

		It("should be deletable", func() {
			By("Deleting task")
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deletion")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task checking environment variables", func() {
		taskName := "e2e-env-test"

		It("should inherit environment variables from target container", func() {
			By("Creating task running 'env'")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"env"},
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to succeed")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.Name).To(Equal(taskName))
				g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Terminated.ExitCode).To(BeZero())
			}, 10*time.Second, 1*time.Second).Should(Succeed())

			By("Verifying stdout contains target container env")
			// Read stdout.log from the executor container (which shares the volume)
			out, err := exec.Command("docker", "exec", ExecutorContainer, "cat", fmt.Sprintf("/tmp/tasks/%s/stdout.log", taskName)).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Failed to read stdout.log: %s", string(out))

			outputStr := string(out)
			Expect(outputStr).To(ContainSubstring("TARGET_VAR=hello-from-target"), "Task environment should inherit from target container")
		})

		It("should be deletable", func() {
			By("Deleting task")
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deletion")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task with timeout", func() {
		taskName := "e2e-timeout-test"

		It("should timeout and be terminated", func() {
			By("Creating task with 5 second timeout that runs for 30 seconds")
			timeoutSec := int64(5)
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command:        []string{"sleep", "30"},
					TimeoutSeconds: &timeoutSec,
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to be terminated (within 15 seconds)")
			// After timeout detection, Stop is called and the process is killed.
			// Once Stop completes, the exit file is written and state becomes Failed.
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.Name).To(Equal(taskName))

				// Should be Terminated with exit code 137 (SIGKILL) or 143 (SIGTERM)
				// sleep responds to SIGTERM quickly, so we usually get 143
				// The state will be "Failed" after exit file is written
				if got.ProcessStatus != nil && got.ProcessStatus.Terminated != nil {
					g.Expect(got.ProcessStatus.Terminated.ExitCode).To(SatisfyAny(
						Equal(int32(137)), // SIGKILL
						Equal(int32(143)), // SIGTERM
					))
				} else {
					// Fail if not terminated yet
					g.Expect(got.ProcessStatus).NotTo(BeNil(), "Task ProcessStatus is nil")
					g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				}
			}, 15*time.Second, 1*time.Second).Should(Succeed())

			By("Verifying the task was terminated")
			got, err := client.Get(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ProcessStatus.Terminated).NotTo(BeNil())
			Expect(got.ProcessStatus.Terminated.ExitCode).To(SatisfyAny(
				Equal(int32(137)), // SIGKILL
				Equal(int32(143)), // SIGTERM
			))
			// State could be "Failed" (after exit file written) or "Timeout" (during stop)
			Expect(got.ProcessStatus.Terminated.Reason).To(SatisfyAny(
				Equal("Failed"),
				Equal("TaskTimeout"),
			))
		})

		It("should be deletable after timeout", func() {
			By("Deleting task")
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deletion")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task that completes before timeout", func() {
		taskName := "e2e-no-timeout-test"

		It("should succeed without timeout", func() {
			By("Creating task with 60 second timeout that completes in 2 seconds")
			timeoutSec := int64(60)
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command:        []string{"sleep", "2"},
					TimeoutSeconds: &timeoutSec,
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to succeed")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.Name).To(Equal(taskName))

				// Should succeed with exit code 0
				if got.ProcessStatus != nil && got.ProcessStatus.Terminated != nil {
					g.Expect(got.ProcessStatus.Terminated.ExitCode).To(BeZero())
					g.Expect(got.ProcessStatus.Terminated.Reason).To(Equal("Succeeded"))
				} else {
					g.Expect(got.ProcessStatus).NotTo(BeNil(), "Task ProcessStatus is nil")
					g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				}
			}, 10*time.Second, 1*time.Second).Should(Succeed())
		})

		It("should be deletable", func() {
			By("Deleting task")
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deletion")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	// ===== Lifecycle Hook E2E Tests =====

	Context("When creating a task with a successful preStart hook", func() {
		taskName := "e2e-prestart-ok"

		It("should execute preStart before main process and succeed", func() {
			By("Creating task with preStart that writes a marker file to shared volume")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					// Main process reads the marker created by preStart via shared volume
					Command: []string{"cat", "/tmp/tasks/prestart-marker"},
					Lifecycle: &api.ProcessLifecycle{
						PreStart: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo prestart-ok > /tmp/tasks/prestart-marker"},
							},
							ExecMode: api.ExecModeLocal,
						},
					},
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to succeed")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.ProcessStatus).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				g.Expect(got.ProcessStatus.Terminated.ExitCode).To(BeZero(),
					"Main process should succeed because preStart created the marker file")
			}, 15*time.Second, 1*time.Second).Should(Succeed())
		})

		It("should be deletable", func() {
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task with a failing preStart hook", func() {
		taskName := "e2e-prestart-fail"

		It("should fail with PreStartHookFailed reason and include stderr", func() {
			By("Creating task with preStart that exits with error")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"echo", "should-not-run"},
					Lifecycle: &api.ProcessLifecycle{
						PreStart: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo 'mount failed: device busy' >&2; exit 1"},
							},
							ExecMode: api.ExecModeLocal,
						},
					},
				},
			}
			_, err := client.Set(context.Background(), task)
			// Set may return error since the task fails immediately, or it may
			// accept the task and report failure via status — both are valid.
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("preStart hook failed"))
			}

			By("Waiting for task to report failure with error details")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.ProcessStatus).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				g.Expect(got.ProcessStatus.Terminated.ExitCode).NotTo(BeZero(),
					"Task should have failed")
				g.Expect(got.ProcessStatus.Terminated.Reason).To(Equal("PreStartHookFailed"),
					"Reason should indicate preStart failure")
				g.Expect(got.ProcessStatus.Terminated.Message).To(ContainSubstring("mount failed: device busy"),
					"Message should contain stderr from the hook")
			}, 10*time.Second, 1*time.Second).Should(Succeed())
		})

		It("should be deletable", func() {
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task with a preStart hook that times out", func() {
		taskName := "e2e-prestart-timeout"

		It("should fail with timeout error", func() {
			By("Creating task with preStart that hangs and a 2s timeout")
			timeoutSec := int64(2)
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"echo", "should-not-run"},
					Lifecycle: &api.ProcessLifecycle{
						PreStart: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "sleep 60"},
							},
							ExecMode:       api.ExecModeLocal,
							TimeoutSeconds: &timeoutSec,
						},
					},
				},
			}

			start := time.Now()
			_, err := client.Set(context.Background(), task)
			// Same as above: error may come inline or via status
			_ = err

			By("Waiting for task to report timeout failure")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.ProcessStatus).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Terminated).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
				g.Expect(got.ProcessStatus.Terminated.Reason).To(Equal("PreStartHookFailed"))
				g.Expect(got.ProcessStatus.Terminated.Message).To(ContainSubstring("timed out"))
			}, 15*time.Second, 1*time.Second).Should(Succeed())

			elapsed := time.Since(start)
			Expect(elapsed).To(BeNumerically("<", 10*time.Second),
				"Should not wait much longer than the 2s hook timeout")
		})

		It("should be deletable", func() {
			_, err := client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 5*time.Second, 500*time.Millisecond).Should(BeNil())
		})
	})

	Context("When creating a task with a postStop hook", func() {
		taskName := "e2e-poststop-ok"

		It("should execute postStop when task is deleted", func() {
			By("Creating a long-running task with postStop that writes a marker file")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"sleep", "60"},
					Lifecycle: &api.ProcessLifecycle{
						PostStop: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo poststop-ok > /tmp/tasks/poststop-marker"},
							},
							ExecMode: api.ExecModeLocal,
						},
					},
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to be running")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.ProcessStatus).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Running).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
			}, 10*time.Second, 1*time.Second).Should(Succeed())

			By("Deleting the task to trigger postStop")
			_, err = client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to be fully deleted")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 10*time.Second, 500*time.Millisecond).Should(BeNil())

			By("Verifying postStop hook executed by checking marker file in executor container")
			out, err := exec.Command("docker", "exec", ExecutorContainer, "cat", "/tmp/tasks/poststop-marker").CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "postStop marker file should exist: %s", string(out))
			Expect(string(out)).To(ContainSubstring("poststop-ok"))
		})
	})

	Context("When creating a task with both preStart and postStop hooks", func() {
		taskName := "e2e-lifecycle-both"

		It("should run preStart → main → postStop in order", func() {
			By("Creating a long-running task where each stage appends to a log file")
			task := &api.Task{
				Name: taskName,
				Process: &api.Process{
					Command: []string{"/bin/sh", "-c", "echo step2-main >> /tmp/tasks/lifecycle-order.log; sleep 60"},
					Lifecycle: &api.ProcessLifecycle{
						PreStart: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo step1-prestart > /tmp/tasks/lifecycle-order.log"},
							},
							ExecMode: api.ExecModeLocal,
						},
						PostStop: &api.LifecycleHandler{
							Exec: &api.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo step3-poststop >> /tmp/tasks/lifecycle-order.log"},
							},
							ExecMode: api.ExecModeLocal,
						},
					},
				},
			}
			_, err := client.Set(context.Background(), task)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to be running (preStart completed)")
			Eventually(func(g Gomega) {
				got, err := client.Get(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(got).NotTo(BeNil())
				g.Expect(got.ProcessStatus).NotTo(BeNil())
				g.Expect(got.ProcessStatus.Running).NotTo(BeNil(), "Task status: %v", got.ProcessStatus)
			}, 10*time.Second, 1*time.Second).Should(Succeed())

			By("Verifying preStart and main have executed")
			out, err := exec.Command("docker", "exec", ExecutorContainer, "cat", "/tmp/tasks/lifecycle-order.log").CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(out)).To(ContainSubstring("step1-prestart"))
			Expect(string(out)).To(ContainSubstring("step2-main"))

			By("Deleting the task to trigger postStop")
			_, err = client.Set(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for task to be fully deleted")
			Eventually(func() *api.Task {
				got, _ := client.Get(context.Background())
				return got
			}, 10*time.Second, 500*time.Millisecond).Should(BeNil())

			By("Verifying postStop hook executed")
			out, err = exec.Command("docker", "exec", ExecutorContainer, "cat", "/tmp/tasks/lifecycle-order.log").CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(out)).To(ContainSubstring("step3-poststop"))
		})
	})
})
