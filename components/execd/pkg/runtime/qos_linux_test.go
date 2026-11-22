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
	require.EqualValues(t, flagQoS, hdr.Flags)
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
	for _, hardened := range []bool{false, true} {
		t.Run(fmt.Sprint(hardened), func(t *testing.T) {
			cfg, probe := setupQoSTest(t)
			if hardened {
				cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
			}
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

			entrypoint := filepath.Join(filepath.Dir(probe), "entrypoint")
			require.NoError(t, os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf '%s' \"$0\" > \"$1\"\n"), 0o755))
			envFile := filepath.Join(t.TempDir(), "env")
			require.NoError(t, os.WriteFile(envFile, []byte("PATH=/nonexistent\n"), 0o600))
			t.Setenv("EXECD_ENVS", envFile)
			outputFile := filepath.Join(t.TempDir(), "entrypoint-output")
			mp, err := launchEntrypoint([]string{"entrypoint", outputFile})
			require.NoError(t, err)
			require.NoError(t, mp.Wait())
			out, err := os.ReadFile(outputFile)
			require.NoError(t, err)
			require.Equal(t, entrypoint, string(out))
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
	cmd := exec.Command(launcherSearchPaths[0], "3", "--", probe, probe)
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
			cmd := exec.Command(launcherSearchPaths[0], "3", "--", probe, probe)
			cmd.ExtraFiles = []*os.File{file}
			out, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.Equal(t, 125, cmd.ProcessState.ExitCode())
			require.Contains(t, strings.ToLower(string(out)), "invalid")
		})
	}
}
