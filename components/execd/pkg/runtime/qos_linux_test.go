//go:build linux

// Copyright 2026 The OpenSandbox Authors
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

package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type qosSample struct {
	Caps       uint64 `json:"caps"`
	Seccomp    int    `json:"seccomp"`
	Nice       int    `json:"nice"`
	Scheduler  int    `json:"scheduler"`
	OOM        int    `json:"oom"`
	OOMAfter   int    `json:"oom_after"`
	Soft       uint64 `json:"soft"`
	Hard       uint64 `json:"hard"`
	NNP        int    `json:"nnp"`
	UID        int    `json:"uid"`
	GID        int    `json:"gid"`
	LowerErrno int    `json:"lower_errno"`
	NiceErrno  int    `json:"nice_errno"`
}

func setupQoSTest(t *testing.T) (isolation.Config, string) {
	t.Helper()
	resetHardening()
	launcherSearchPaths = []string{buildLauncher(t)}
	t.Cleanup(func() {
		qos.enabled.Store(false)
		qos.config = isolation.QoSConfig{}
		qos.launcherPath = ""
		resetHardening()
	})
	// The probe must remain executable after a launcher-side UID change.
	dir, err := os.MkdirTemp("", "qos-probe-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.Chmod(dir, 0o755))
	probe := filepath.Join(dir, "probe")
	out, err := exec.Command("cc", "-O2", "-Wall", "-Wextra", "-Werror", "-o", probe,
		filepath.Join("..", "..", "native", "qos_probe_test.c")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	cfg := isolation.DefaultConfig()
	cfg.QoS.Enabled = true
	return cfg, probe
}

func readQoSSample(t *testing.T, data []byte) qosSample {
	t.Helper()
	var sample qosSample
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &sample), "%s", data)
	return sample
}

func runQoSProbe(t *testing.T, cmd *exec.Cmd, opts ...launchOption) qosSample {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	mp, err := launchManaged(cmd, opts...)
	require.NoError(t, err)
	require.NoError(t, mp.Wait(), "%s", stderr.String())
	require.Empty(t, stderr.String())
	return readQoSSample(t, stdout.Bytes())
}

func TestQoSDefaultsAndHardeningIndependence(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	baseline := runQoSProbe(t, exec.Command(probe))
	cfg.QoS.Enabled = false
	require.NoError(t, InitQoS(cfg))
	require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe)))

	cfg.QoS.Enabled = true
	require.NoError(t, InitQoS(cfg))
	got := runQoSProbe(t, exec.Command(probe))
	require.Equal(t, 19, got.Nice)
	require.Equal(t, unix.SCHED_NORMAL, got.Scheduler)
	require.Equal(t, max(baseline.OOM, 500), got.OOM)
	require.Zero(t, got.Soft)
	require.Zero(t, got.Hard)
	require.Equal(t, baseline.NNP, got.NNP)
	require.Equal(t, baseline.Caps, got.Caps)
	require.Equal(t, baseline.Seccomp, got.Seccomp)
	require.Equal(t, "disabled", ReportHardening().CapDrop.State)

	out, err := exec.Command(probe).Output()
	require.NoError(t, err)
	require.Equal(t, baseline, readQoSSample(t, out), "execd and bootstrap launches must remain unchanged")

	cfg.QoS.Enabled = false
	require.NoError(t, InitQoS(cfg))
	require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe)))
}

func TestQoSSchedulerAndOOMOptions(t *testing.T) {
	for _, scheduler := range []string{"other", "idle"} {
		t.Run(scheduler, func(t *testing.T) {
			cfg, probe := setupQoSTest(t)
			baseline := runQoSProbe(t, exec.Command(probe))
			cfg.QoS.Scheduler = scheduler
			cfg.QoS.Nice = 10
			cfg.QoS.OOMScoreAdj = 0
			require.NoError(t, InitQoS(cfg))
			got := runQoSProbe(t, exec.Command(probe))
			require.Equal(t, baseline.OOM, got.OOM)
			if scheduler == "idle" {
				require.Equal(t, unix.SCHED_IDLE, got.Scheduler)
				require.Equal(t, baseline.Nice, got.Nice)
			} else {
				require.Equal(t, unix.SCHED_NORMAL, got.Scheduler)
				require.Equal(t, 10, got.Nice)
			}
		})
	}
}

func TestQoSEntrypointTiersKeepHardening(t *testing.T) {
	for _, hardened := range []bool{false, true} {
		for _, tier := range []string{"standard", "high"} {
			t.Run(fmt.Sprintf("%s/hardening=%v", tier, hardened), func(t *testing.T) {
				cfg, probe := setupQoSTest(t)
				baseline := runQoSProbe(t, exec.Command(probe))
				cfg.QoS.Entrypoint = tier
				if hardened {
					cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
				}
				require.NoError(t, InitHardening(cfg))
				require.NoError(t, InitQoS(cfg))
				output := filepath.Join(t.TempDir(), "entrypoint.json")
				mp, err := launchEntrypoint([]string{"/bin/sh", "-c", fmt.Sprintf("exec %s > %s", probe, output)})
				require.NoError(t, err)
				require.NoError(t, mp.Wait())
				data, err := os.ReadFile(output)
				require.NoError(t, err)
				got := readQoSSample(t, data)
				if tier == "standard" {
					require.Equal(t, 19, got.Nice)
					require.Equal(t, max(baseline.OOM, 500), got.OOM)
				} else {
					require.Equal(t, baseline.Nice, got.Nice)
					require.Equal(t, baseline.OOM, got.OOM)
					require.Equal(t, baseline.Soft, got.Soft)
					require.Equal(t, baseline.Hard, got.Hard)
				}
				if hardened {
					require.Equal(t, 1, got.NNP)
					require.Zero(t, got.Caps)
					require.Equal(t, 2, got.Seccomp)
				} else {
					require.Equal(t, baseline.NNP, got.NNP)
				}
				require.Equal(t, 19, runQoSProbe(t, exec.Command(probe)).Nice)
			})
		}
	}
}

func TestQoSIdentityAndUnprivilegedAdjustment(t *testing.T) {
	for _, hardened := range []bool{false, true} {
		t.Run(fmt.Sprint(hardened), func(t *testing.T) {
			cfg, probe := setupQoSTest(t)
			baseline := runQoSProbe(t, exec.Command(probe))
			if baseline.OOM == 1000 {
				t.Skip("inherited OOM adjustment is already at the kernel maximum")
			}
			cfg.QoS.OOMScoreAdj = max(500, baseline.OOM+1)
			if hardened {
				cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
			}
			require.NoError(t, InitHardening(cfg))
			require.NoError(t, InitQoS(cfg))
			cmd := exec.Command(probe, "lower", strconv.Itoa(baseline.OOM))
			uid, gid := os.Getuid(), os.Getgid()
			if os.Geteuid() == 0 {
				uid, gid = 65534, 65534
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
			}
			got := runQoSProbe(t, cmd)
			require.Equal(t, uid, got.UID)
			require.Equal(t, gid, got.GID)
			require.Equal(t, 19, got.Nice)
			require.Equal(t, cfg.QoS.OOMScoreAdj, got.OOM)
			require.Zero(t, got.LowerErrno, "initial unprivileged raise must not install a new minimum")
			require.Equal(t, baseline.OOM, got.OOMAfter)
			require.NotZero(t, got.NiceErrno, "unprivileged workload must not raise its priority")
		})
	}
}

func TestQoSInheritedOOMNeverLowered(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	baseline := runQoSProbe(t, exec.Command(probe))
	inherited := max(800, baseline.OOM)
	require.NoError(t, InitQoS(cfg))
	cmd := exec.Command(probe)
	policy, err := hardenCmd(cmd, false, false, nil)
	require.NoError(t, err)
	defer policy.Close()
	cmd.Args = append([]string{probe, "inherit", strconv.Itoa(inherited)}, cmd.Args...)
	cmd.Path = probe
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Equal(t, inherited, readQoSSample(t, out).OOM)
}

func TestQoSUnsupportedOperationsPreserveHardening(t *testing.T) {
	for _, inheritedFloor := range []int{0, 1000} {
		t.Run(fmt.Sprintf("inherited-floor=%d", inheritedFloor), func(t *testing.T) {
			cfg, probe := setupQoSTest(t)
			baseline := runQoSProbe(t, exec.Command(probe))
			inherited := max(inheritedFloor, baseline.OOM)
			cfg.QoS.OOMScoreAdj = 1000
			cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
			require.NoError(t, InitHardening(cfg))
			require.NoError(t, InitQoS(cfg))
			cmd := exec.Command(probe)
			policy, err := hardenCmd(cmd, false, false, nil)
			require.NoError(t, err)
			defer policy.Close()
			cmd.Args = append([]string{probe, "inherit", strconv.Itoa(inherited), probe, "deny-qos"}, cmd.Args...)
			cmd.Path = probe
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Run(), "%s", stderr.String())
			got := readQoSSample(t, stdout.Bytes())
			require.Equal(t, 1, got.NNP)
			require.Zero(t, got.Caps)
			require.Equal(t, 2, got.Seccomp)
			require.Zero(t, got.Soft)
			require.Zero(t, got.Hard)
			require.Equal(t, inherited, got.OOM)
			for _, operation := range []string{"scheduler", "nice"} {
				require.Contains(t, stderr.String(), "qos: "+operation)
			}
			if inherited < cfg.QoS.OOMScoreAdj {
				require.Contains(t, stderr.String(), "qos: write oom_score_adj")
			} else {
				require.NotContains(t, stderr.String(), "qos: write oom_score_adj")
			}
		})
	}
}

func TestQoSNamespaceAndInternalHelperPolicies(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	baseline := runQoSProbe(t, exec.Command(probe))
	cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
	cfg.QoS.Entrypoint = "high"
	require.NoError(t, InitHardening(cfg))
	require.NoError(t, InitQoS(cfg))
	got := runQoSProbe(t, exec.Command(probe), withoutHardening())
	require.Equal(t, 19, got.Nice)
	require.Equal(t, baseline.NNP, got.NNP, "namespace builder must not receive the hardening floor")
	require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe), withoutHardening(), withoutQoS()))
}

func TestQoSPTYAndShellSessions(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	cfg.QoS.Entrypoint = "high"
	require.NoError(t, InitQoS(cfg))
	for _, mode := range []string{"pty", "pipe"} {
		t.Run(mode, func(t *testing.T) {
			s := newPTYSession(uuidString(), "", "exec "+probe)
			if mode == "pty" {
				require.NoError(t, s.StartPTY())
			} else {
				require.NoError(t, s.StartPipe())
			}
			t.Cleanup(func() { s.close() })
			select {
			case <-s.OutputDone():
			case <-time.After(5 * time.Second):
				t.Fatal("session did not finish")
			}
			data, _ := s.replay.ReadFrom(0)
			require.Equal(t, 19, readQoSSample(t, data).Nice)
		})
	}
	t.Run("bash", func(t *testing.T) {
		s := newBashSession("", newBashSessionEnv())
		require.NoError(t, s.start())
		t.Cleanup(func() { _ = s.close() })
		var output strings.Builder
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.run(ctx, &ExecuteCodeRequest{
			Code:  probe,
			Hooks: ExecuteResultHook{OnExecuteStdout: func(line string) { output.WriteString(line) }},
		}))
		require.Equal(t, 19, readQoSSample(t, []byte(output.String())).Nice)
	})
	t.Run("lifecycle managed command", func(t *testing.T) {
		var out bytes.Buffer
		cmd := exec.Command(probe)
		cmd.Stdout = &out
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code, err := RunManagedCommand(ctx, cmd, func() { _ = cmd.Process.Kill() })
		require.NoError(t, err)
		require.Zero(t, code)
		require.Equal(t, 19, readQoSSample(t, out.Bytes()).Nice)
	})
}

func TestQoSCommandAndIsolatedSessionRouting(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	cfg.QoS.Entrypoint = "high"
	require.NoError(t, InitQoS(cfg))
	t.Run("command", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		var output strings.Builder
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctrl := NewController("", "")
		require.NoError(t, ctrl.runCommand(ctx, &ExecuteCodeRequest{
			Code: probe, Cwd: t.TempDir(), Timeout: 5 * time.Second,
			Hooks: ExecuteResultHook{
				OnExecuteInit:     func(string) {},
				OnExecuteStdout:   func(line string) { output.WriteString(line) },
				OnExecuteComplete: func(time.Duration) {},
			},
		}))
		require.Equal(t, 19, readQoSSample(t, []byte(output.String())).Nice)
	})
	t.Run("isolated workload launch", func(t *testing.T) {
		useDirectProcessKill(t)
		isolator := &lifecycleHarnessIsolator{lifecycle: newLifecycleHarness()}
		session := newLifecycleRuntimeSession(t, isolator, nil)
		require.NoError(t, session.start())
		t.Cleanup(func() { _ = session.stop() })
		priority, err := unix.Getpriority(unix.PRIO_PROCESS, isolator.cmd.Process.Pid)
		require.NoError(t, err)
		require.Equal(t, 1, priority, "Linux getpriority returns 20 - nice")
	})
}

func TestQoSMissingLauncherAndInvalidConfiguration(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	baseline := runQoSProbe(t, exec.Command(probe))
	launcherSearchPaths = []string{filepath.Join(t.TempDir(), "missing-launcher")}
	t.Setenv("PATH", "")
	require.NoError(t, InitQoS(cfg))
	require.False(t, qos.enabled.Load())
	require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe)))
	cfg.QoS.Nice = 0
	require.ErrorContains(t, InitQoS(cfg), "nice")
}

func TestQoSNonExecutableLauncherFallsBackToDirectLaunch(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	validLauncher := launcherSearchPaths[0]
	baseline := runQoSProbe(t, exec.Command(probe))

	t.Run("mode-0644-file", func(t *testing.T) {
		nonExec := filepath.Join(t.TempDir(), "opensandbox-launcher")
		data, err := os.ReadFile(validLauncher)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(nonExec, data, 0o644))
		// Keep a valid launcher on PATH to ensure a non-executable fixed-path
		// binary degrades instead of falling back to PATH.
		t.Setenv("PATH", filepath.Dir(validLauncher))
		launcherSearchPaths = []string{nonExec}

		require.NoError(t, InitQoS(cfg))
		require.False(t, qos.enabled.Load())
		require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe)))

		mp, err := launchEntrypoint([]string{"/bin/echo", "ok"})
		require.NoError(t, err)
		require.NoError(t, mp.Wait())
	})

	t.Run("directory", func(t *testing.T) {
		launcherSearchPaths = []string{t.TempDir()}
		t.Setenv("PATH", filepath.Dir(validLauncher))

		require.NoError(t, InitQoS(cfg))
		require.False(t, qos.enabled.Load())
		require.Equal(t, baseline, runQoSProbe(t, exec.Command(probe)))
	})
}

func TestQoSIdentityFailureStillAborts(t *testing.T) {
	for _, hardened := range []bool{false, true} {
		t.Run(fmt.Sprint(hardened), func(t *testing.T) {
			cfg, probe := setupQoSTest(t)
			if hardened {
				cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
			}
			require.NoError(t, InitHardening(cfg))
			require.NoError(t, InitQoS(cfg))
			cmd := exec.Command(probe)
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
				Uid: ^uint32(0), Gid: uint32(os.Getgid()), NoSetGroups: true,
			}}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			mp, err := launchManaged(cmd)
			if hardened {
				require.NoError(t, err)
				require.Error(t, mp.Wait())
				require.Equal(t, 125, cmd.ProcessState.ExitCode())
				require.Contains(t, stderr.String(), "cannot")
			} else {
				require.Error(t, err)
				require.Nil(t, mp)
				require.Nil(t, cmd.Process)
			}
			require.Empty(t, stdout.String(), "workload must not run with the wrong identity")
		})
	}
}

func TestQoSWithInitReaper(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	require.NoError(t, InitQoS(cfg))
	startReaperForTest(t)
	output, err := os.CreateTemp(t.TempDir(), "probe")
	require.NoError(t, err)
	defer output.Close()
	cmd := exec.Command(probe)
	cmd.Stdout, cmd.Stderr = output, output
	mp, err := launchManaged(cmd)
	require.NoError(t, err)
	require.NoError(t, mp.Wait())
	data, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	require.Equal(t, 19, readQoSSample(t, data).Nice)
}

func TestQoSExecutableStartFailures(t *testing.T) {
	cfg, _ := setupQoSTest(t)
	dir := t.TempDir()
	for _, withInit := range []bool{false, true} {
		t.Run(fmt.Sprintf("init=%v", withInit), func(t *testing.T) {
			if withInit {
				startReaperForTest(t)
			}
			for _, tc := range []struct {
				name    string
				content string
				mode    os.FileMode
				errno   syscall.Errno
			}{
				{name: "missing", errno: syscall.ENOENT},
				{name: "not-executable", content: "#!/bin/sh\nexit 0\n", mode: 0o644, errno: syscall.EACCES},
				{name: "missing-interpreter", content: "#!" + filepath.Join(dir, "missing") + "\n", mode: 0o755, errno: syscall.ENOENT},
				{name: "invalid-format", content: "not an executable\n", mode: 0o755, errno: syscall.ENOEXEC},
			} {
				t.Run(tc.name, func(t *testing.T) {
					path := filepath.Join(dir, tc.name)
					if tc.content != "" {
						require.NoError(t, os.WriteFile(path, []byte(tc.content), tc.mode))
					}
					for _, enabled := range []bool{false, true} {
						cfg.QoS.Enabled = enabled
						require.NoError(t, InitQoS(cfg))
						before, err := os.ReadDir("/proc/self/fd")
						require.NoError(t, err)
						cmd := exec.Command(path)
						stdout, err := cmd.StdoutPipe()
						require.NoError(t, err)
						stderr, err := cmd.StderrPipe()
						require.NoError(t, err)
						var pid int
						mp, err := launchManagedWith(cmd, func() error {
							err := cmd.Start()
							if err == nil {
								pid = cmd.Process.Pid
							}
							return err
						})
						require.ErrorIs(t, err, tc.errno)
						var pathErr *os.PathError
						require.ErrorAs(t, err, &pathErr)
						require.Equal(t, "fork/exec", pathErr.Op)
						require.Equal(t, path, pathErr.Path)
						require.Nil(t, mp)
						require.Nil(t, cmd.Process)
						var buf [1]byte
						_, err = stdout.Read(buf[:])
						require.ErrorIs(t, err, os.ErrClosed)
						_, err = stderr.Read(buf[:])
						require.ErrorIs(t, err, os.ErrClosed)
						if pid != 0 {
							_, err = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
							require.ErrorIs(t, err, syscall.ECHILD)
						}
						if withInit {
							initReaper.mu.Lock()
							remaining := len(initReaper.owned)
							initReaper.mu.Unlock()
							require.Zero(t, remaining)
						}
						after, err := os.ReadDir("/proc/self/fd")
						require.NoError(t, err)
						require.LessOrEqual(t, len(after), len(before), "failed launch must not leak descriptors")
					}
				})
			}
		})
	}
}

func TestQoSExecHandshakeClosesBeforeWorkloadExit(t *testing.T) {
	cfg, _ := setupQoSTest(t)
	require.NoError(t, InitQoS(cfg))
	for _, withInit := range []bool{false, true} {
		t.Run(fmt.Sprintf("init=%v", withInit), func(t *testing.T) {
			if withInit {
				startReaperForTest(t)
			}
			input, writer, err := os.Pipe()
			require.NoError(t, err)
			defer input.Close()
			defer writer.Close()
			cmd := exec.Command("/bin/sh", "-c", "test -e /proc/self/fd/3 || exit 98; test ! -e /proc/self/fd/5 || exit 99; read -r line; exit 126")
			cmd.Stdin = input
			cmd.ExtraFiles = []*os.File{input}
			type result struct {
				process *managedProcess
				err     error
			}
			started := make(chan result, 1)
			go func() {
				mp, err := launchManaged(cmd)
				started <- result{mp, err}
			}()
			var got result
			select {
			case got = <-started:
			case <-time.After(5 * time.Second):
				_ = writer.Close()
				got = <-started
				if got.process != nil {
					_ = got.process.Wait()
				}
				t.Fatal("startup waited for the workload instead of exec")
			}
			require.NoError(t, got.err)
			require.NotNil(t, got.process)
			_, err = writer.WriteString("exit\n")
			require.NoError(t, err)
			require.Error(t, got.process.Wait())
			require.Equal(t, 126, got.process.ExitCode())
		})
	}
}

func TestQoSExecutableStartFailureRouting(t *testing.T) {
	cfg, _ := setupQoSTest(t)
	require.NoError(t, InitQoS(cfg))
	path := filepath.Join(t.TempDir(), "missing")
	for _, withInit := range []bool{false, true} {
		t.Run(fmt.Sprintf("init=%v", withInit), func(t *testing.T) {
			if withInit {
				startReaperForTest(t)
			}
			t.Run("entrypoint", func(t *testing.T) {
				mp, err := launchEntrypoint([]string{path})
				require.ErrorIs(t, err, syscall.ENOENT)
				require.Nil(t, mp)
			})
			t.Run("lifecycle", func(t *testing.T) {
				code, err := RunManagedCommand(context.Background(), exec.Command(path), func() {
					t.Error("failed startup must not invoke cancellation")
				})
				require.ErrorIs(t, err, syscall.ENOENT)
				require.Equal(t, -1, code)
			})
			t.Run("command", func(t *testing.T) {
				t.Setenv("TMPDIR", t.TempDir())
				var executionError *execute.ErrorOutput
				var completed bool
				req := &ExecuteCodeRequest{
					Argv: []string{path},
					Hooks: ExecuteResultHook{
						OnExecuteError:    func(e *execute.ErrorOutput) { executionError = e },
						OnExecuteComplete: func(time.Duration) { completed = true },
					},
				}
				req.SetDefaultHooks()
				require.NoError(t, NewController("", "").runCommand(context.Background(), req))
				require.NotNil(t, executionError)
				require.Equal(t, "CommandExecError", executionError.EName)
				require.Contains(t, executionError.EValue, path)
				require.Contains(t, executionError.EValue, syscall.ENOENT.Error())
				require.False(t, completed)
			})
		})
	}
}

func TestQoSExecFailureClosesPTY(t *testing.T) {
	cfg, _ := setupQoSTest(t)
	require.NoError(t, InitQoS(cfg))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bash"), []byte("#!"+filepath.Join(dir, "missing")+"\n"), 0o755))
	t.Setenv("PATH", dir)
	resetShellCacheForTest()
	t.Cleanup(resetShellCacheForTest)
	for _, withInit := range []bool{false, true} {
		t.Run(fmt.Sprintf("init=%v", withInit), func(t *testing.T) {
			if withInit {
				startReaperForTest(t)
			}
			for _, mode := range []string{"pty", "pipe"} {
				t.Run(mode, func(t *testing.T) {
					before, err := os.ReadDir("/proc/self/fd")
					require.NoError(t, err)
					for range 3 {
						s := newPTYSession(uuidString(), "", "")
						if mode == "pty" {
							err = s.StartPTY()
						} else {
							err = s.StartPipe()
						}
						require.ErrorIs(t, err, syscall.ENOENT)
						require.Nil(t, s.proc)
					}
					after, err := os.ReadDir("/proc/self/fd")
					require.NoError(t, err)
					require.LessOrEqual(t, len(after), len(before), "failed session must not leak descriptors")
				})
			}
		})
	}
}

func TestQoSPolicyEncodingAndCredentialPreservation(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	cfg.QoS.Scheduler = "idle"
	require.NoError(t, InitQoS(cfg))
	cmd := exec.Command(probe)
	cred := &syscall.Credential{Uid: 123, Gid: 456, Groups: []uint32{789}, NoSetGroups: true}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	policy, err := hardenCmd(cmd, true, false, nil)
	require.NoError(t, err)
	defer policy.Close()
	var hdr policyHeader
	require.NoError(t, binary.Read(policy, binary.LittleEndian, &hdr))
	require.EqualValues(t, policyVersion, hdr.Version)
	require.EqualValues(t, flagQoS|flagExecPath, hdr.Flags)
	require.EqualValues(t, unix.SCHED_IDLE, hdr.QoSScheduler)
	require.EqualValues(t, 19, hdr.QoSNice)
	require.EqualValues(t, 500, hdr.QoSOOMScoreAdj)
	require.Zero(t, hdr.UID)
	require.Zero(t, hdr.GID)
	require.Zero(t, hdr.NGroups)
	require.Zero(t, hdr.SeccompLen)
	require.Zero(t, hdr.LandlockLen)
	require.Zero(t, hdr.NEnv, "QoS alone must not change credential/environment policy")
	require.Same(t, cred, cmd.SysProcAttr.Credential)
}

func TestQoSResolvedExecutableAndArgv(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	require.NoError(t, InitHardening(cfg))
	require.NoError(t, InitQoS(cfg))
	t.Setenv("PATH", filepath.Dir(probe))
	for _, argv0 := range []string{"probe", "custom-argv0"} {
		cmd := exec.Command("probe", "argv0")
		cmd.Args[0] = argv0
		cmd.Env = []string{"PATH=/nonexistent"}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		mp, err := launchManaged(cmd)
		require.NoError(t, err)
		require.NoError(t, mp.Wait(), "%s", output.String())
		require.Equal(t, argv0+"\n", output.String())
	}
	cmd := &exec.Cmd{Path: probe}
	require.Equal(t, 19, runQoSProbe(t, cmd).Nice)

	daemonDir := filepath.Dir(probe)
	overlayDir := t.TempDir()
	daemonEntrypoint := filepath.Join(daemonDir, "entrypoint")
	overlayEntrypoint := filepath.Join(overlayDir, "entrypoint")
	require.NoError(t, os.WriteFile(daemonEntrypoint, []byte("#!/bin/sh\nprintf '%s' \"$0\" > \"$1\"\n"), 0o755))
	require.NoError(t, os.WriteFile(overlayEntrypoint, []byte("#!/bin/sh\nprintf '%s' \"$0\" > \"$1\"\n"), 0o755))

	for _, overlayPATH := range []string{"/nonexistent", overlayDir} {
		envFile := filepath.Join(t.TempDir(), "env")
		require.NoError(t, os.WriteFile(envFile, []byte("PATH="+overlayPATH+"\n"), 0o600))
		t.Setenv("EXECD_ENVS", envFile)
		outputFile := filepath.Join(t.TempDir(), "entrypoint-output")
		mp, err := launchEntrypoint([]string{"entrypoint", outputFile})
		require.NoError(t, err)
		require.NoError(t, mp.Wait())
		out, err := os.ReadFile(outputFile)
		require.NoError(t, err)
		require.Equal(t, daemonEntrypoint, string(out))
	}
}

func TestHardeningEntrypointPreservesOverlayPATHLookup(t *testing.T) {
	for _, qosEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("qos=%v", qosEnabled), func(t *testing.T) {
			cfg, _ := setupQoSTest(t)
			cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
			cfg.QoS.Enabled = qosEnabled
			require.NoError(t, InitHardening(cfg))
			require.NoError(t, InitQoS(cfg))

			daemonDir := t.TempDir()
			overlayDir := t.TempDir()
			require.NoError(t, os.Chmod(daemonDir, 0o755))
			require.NoError(t, os.Chmod(overlayDir, 0o755))
			daemonCmd := filepath.Join(daemonDir, "same-cmd")
			overlayCmd := filepath.Join(overlayDir, "same-cmd")
			require.NoError(t, os.WriteFile(daemonCmd, []byte("#!/bin/sh\nprintf '%s' \"$0\" > \"$1\"\n"), 0o755))
			require.NoError(t, os.WriteFile(overlayCmd, []byte("#!/bin/sh\nprintf '%s' \"$0\" > \"$1\"\n"), 0o755))

			t.Setenv("PATH", daemonDir)
			envFile := filepath.Join(t.TempDir(), "env")
			require.NoError(t, os.WriteFile(envFile, []byte("PATH="+overlayDir+"\n"), 0o600))
			t.Setenv("EXECD_ENVS", envFile)

			outputDir := t.TempDir()
			require.NoError(t, os.Chmod(outputDir, 0o777))
			outputFile := filepath.Join(outputDir, "out")
			mp, err := launchEntrypoint([]string{"same-cmd", outputFile})
			require.NoError(t, err)
			require.NoError(t, mp.Wait())
			out, err := os.ReadFile(outputFile)
			require.NoError(t, err)
			require.Equal(t, overlayCmd, string(out))
		})
	}
}

func TestQoSSupplementaryGroups(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	groups := make([]uint32, 128)
	for i := range groups {
		groups[i] = uint32(i + 2000)
	}
	for _, noSetGroups := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-setgroups=%v", noSetGroups), func(t *testing.T) {
			var baseline []uint32
			for _, enabled := range []bool{false, true} {
				cfg.QoS.Enabled = enabled
				require.NoError(t, InitQoS(cfg))
				cred := &syscall.Credential{
					Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()),
					Groups: groups, NoSetGroups: noSetGroups,
				}
				cmd := exec.Command(probe, "groups")
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
				var output bytes.Buffer
				cmd.Stdout, cmd.Stderr = &output, &output
				mp, err := launchManaged(cmd)
				if !noSetGroups && !effectiveCapsHave(unix.CAP_SETGID) {
					require.ErrorIs(t, err, os.ErrPermission)
					require.Nil(t, mp)
					require.Nil(t, cmd.Process)
					require.ErrorContains(t, credentialStartHint(err, cred), "requires CAP_SETUID/CAP_SETGID")
					continue
				}
				require.NoError(t, err)
				require.NoError(t, mp.Wait(), "%s", output.String())
				var got []uint32
				require.NoError(t, json.Unmarshal(output.Bytes(), &got))
				if !noSetGroups {
					require.Equal(t, groups, got)
				}
				if enabled {
					require.Equal(t, baseline, got)
				} else {
					baseline = got
				}
			}
		})
	}
}

func TestQoSSeccompImpliesNoNewPrivs(t *testing.T) {
	_, probe := setupQoSTest(t)
	var filter bytes.Buffer
	require.NoError(t, binary.Write(&filter, binary.LittleEndian, unix.SockFilter{
		Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW,
	}))
	data, err := encodePolicy(&hardeningPolicy{seccomp: filter.Bytes()})
	require.NoError(t, err)
	fd, err := createPolicyMemfd(data)
	require.NoError(t, err)
	file := os.NewFile(uintptr(fd), "seccomp-policy")
	defer file.Close()
	cmd := exec.Command(launcherSearchPaths[0], "3", "-1", "--", probe, probe)
	cmd.ExtraFiles = []*os.File{file}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	got := readQoSSample(t, out)
	require.Equal(t, 1, got.NNP)
	require.Equal(t, 2, got.Seccomp)
}

func TestQoSInvalidPolicyRejected(t *testing.T) {
	_, probe := setupQoSTest(t)
	for _, version := range []uint32{1, policyVersion} {
		t.Run(strconv.Itoa(int(version)), func(t *testing.T) {
			var data bytes.Buffer
			require.NoError(t, binary.Write(&data, binary.LittleEndian, policyHeader{
				Magic: policyMagic, Version: version, Flags: flagQoS, QoSNice: 20,
			}))
			fd, err := createPolicyMemfd(data.Bytes())
			require.NoError(t, err)
			file := os.NewFile(uintptr(fd), "invalid-policy")
			defer file.Close()
			cmd := exec.Command(launcherSearchPaths[0], "3", "-1", "--", probe, probe)
			cmd.ExtraFiles = []*os.File{file}
			out, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.Equal(t, 125, cmd.ProcessState.ExitCode())
			require.Contains(t, strings.ToLower(string(out)), "invalid")
		})
	}
}
