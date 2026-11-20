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
			if baseline.OOM >= 500 {
				t.Skip("fixture needs inherited OOM adjustment below the target")
			}
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
			require.Equal(t, 500, got.OOM)
			require.Zero(t, got.LowerErrno, "initial unprivileged raise must not install a new minimum")
			require.Equal(t, baseline.OOM, got.OOMAfter)
			require.NotZero(t, got.NiceErrno, "unprivileged workload must not raise its priority")
		})
	}
}

func TestQoSInheritedOOMNeverLowered(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	require.NoError(t, InitQoS(cfg))
	cmd := exec.Command(probe)
	policy, err := hardenCmd(cmd, false, false, nil)
	require.NoError(t, err)
	defer policy.Close()
	cmd.Args = append([]string{probe, "inherit", "800"}, cmd.Args...)
	cmd.Path = probe
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Equal(t, 800, readQoSSample(t, out).OOM)
}

func TestQoSUnsupportedOperationsPreserveHardening(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
	require.NoError(t, InitHardening(cfg))
	require.NoError(t, InitQoS(cfg))
	cmd := exec.Command(probe)
	policy, err := hardenCmd(cmd, false, false, nil)
	require.NoError(t, err)
	defer policy.Close()
	cmd.Args = append([]string{probe, "deny-qos"}, cmd.Args...)
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
	for _, operation := range []string{"scheduler", "nice", "write oom_score_adj"} {
		require.Contains(t, stderr.String(), "qos: "+operation)
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
			require.NoError(t, err)
			require.Error(t, mp.Wait())
			require.Equal(t, 125, cmd.ProcessState.ExitCode())
			require.Empty(t, stdout.String(), "workload must not run with the wrong identity")
			require.Contains(t, stderr.String(), "cannot")
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

func TestQoSPolicyEncodingAndCredentialPreservation(t *testing.T) {
	cfg, probe := setupQoSTest(t)
	cfg.QoS.Scheduler = "idle"
	require.NoError(t, InitQoS(cfg))
	cmd := exec.Command(probe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 123, Gid: 456, NoSetGroups: true}}
	policy, err := hardenCmd(cmd, true, false, nil)
	require.NoError(t, err)
	defer policy.Close()
	var hdr policyHeader
	require.NoError(t, binary.Read(policy, binary.LittleEndian, &hdr))
	require.EqualValues(t, policyVersion, hdr.Version)
	require.EqualValues(t, flagQoS|flagUIDDrop|flagNoSetGroups, hdr.Flags)
	require.EqualValues(t, unix.SCHED_IDLE, hdr.QoSScheduler)
	require.EqualValues(t, 19, hdr.QoSNice)
	require.EqualValues(t, 500, hdr.QoSOOMScoreAdj)
	require.EqualValues(t, 123, hdr.UID)
	require.EqualValues(t, 456, hdr.GID)
	require.Zero(t, hdr.SeccompLen)
	require.Zero(t, hdr.LandlockLen)
	require.Zero(t, hdr.NEnv, "QoS alone must not change credential/environment policy")
	require.Nil(t, cmd.SysProcAttr.Credential)
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
			cmd := exec.Command(launcherSearchPaths[0], "3", "--", probe)
			cmd.ExtraFiles = []*os.File{file}
			out, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.Equal(t, 125, cmd.ProcessState.ExitCode())
			require.Contains(t, strings.ToLower(string(out)), "invalid")
		})
	}
}
