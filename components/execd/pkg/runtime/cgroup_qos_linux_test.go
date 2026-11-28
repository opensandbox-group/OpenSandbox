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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/cgroupfs"
	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

func setupFakeCgroupV2Env(t *testing.T, controllers string) (baseDir string) {
	t.Helper()
	tmp := t.TempDir()
	baseDir = filepath.Join(tmp, "cgroup")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "cgroup.controllers"), []byte(controllers+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "cgroup.subtree_control"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "pids.max"), []byte("1024\n"), 0o644))

	procCgroup := filepath.Join(tmp, "proc_cgroup")
	require.NoError(t, os.WriteFile(procCgroup, []byte("0::/\n"), 0o644))
	procMountinfo := filepath.Join(tmp, "proc_mountinfo")
	require.NoError(t, os.WriteFile(procMountinfo, nil, 0o644))
	procFilesystems := filepath.Join(tmp, "proc_filesystems")
	require.NoError(t, os.WriteFile(procFilesystems, []byte("nodev\tcgroup2\n"), 0o644))
	procMeminfo := filepath.Join(tmp, "proc_meminfo")
	require.NoError(t, os.WriteFile(procMeminfo, []byte("MemTotal:       1048576 kB\n"), 0o644))

	origDefaultMount := cgroupQoSDefaultMount
	origProcCgroup := cgroupQoSProcCgroup
	origProcMountinfo := cgroupQoSProcMountinfo
	origProcFilesystems := cgroupQoSProcFilesystems
	origProcMeminfo := cgroupQoSProcMeminfo
	origScan := cgroupQoSScanV2Mount
	origMount := cgroupQoSMount
	origNumCPU := cgroupQoSNumCPU
	origSelfPID := cgroupQoSSelfPID

	cgroupQoSDefaultMount = baseDir
	cgroupQoSProcCgroup = procCgroup
	cgroupQoSProcMountinfo = procMountinfo
	cgroupQoSProcFilesystems = procFilesystems
	cgroupQoSProcMeminfo = procMeminfo
	cgroupQoSScanV2Mount = func() (string, error) { return baseDir, nil }
	cgroupQoSMount = func(string) error { return nil }
	cgroupQoSNumCPU = func() int { return 2 }
	cgroupQoSSelfPID = func() int { return 4242 }

	t.Cleanup(func() {
		cgroupQoS.reset()
		cgroupQoSDefaultMount = origDefaultMount
		cgroupQoSProcCgroup = origProcCgroup
		cgroupQoSProcMountinfo = origProcMountinfo
		cgroupQoSProcFilesystems = origProcFilesystems
		cgroupQoSProcMeminfo = origProcMeminfo
		cgroupQoSScanV2Mount = origScan
		cgroupQoSMount = origMount
		cgroupQoSNumCPU = origNumCPU
		cgroupQoSSelfPID = origSelfPID
	})
	return baseDir
}

func TestInitCgroupQoS_HappyPath(t *testing.T) {
	baseDir := setupFakeCgroupV2Env(t, "cpu memory pids")

	cfg := isolation.DefaultConfig()
	cfg.QoS.Entrypoint = "high"
	cfg.QoS.Cgroup.Enabled = true
	cfg.QoS.Cgroup.WorkloadCPUMaxPercent = 80

	require.NoError(t, InitCgroupQoS(cfg, false))
	require.True(t, cgroupQoS.enabled.Load())
	assert.True(t, cgroupQoS.isHighEntrypoint())

	sysDir := filepath.Join(baseDir, cgroupSystemDirName)
	wkDir := filepath.Join(baseDir, cgroupWorkloadDirName)

	readFile := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		return string(b)
	}

	assert.Equal(t, "4242", readFile(filepath.Join(sysDir, "cgroup.procs")))
	assert.Equal(t, "1000", readFile(filepath.Join(sysDir, "cpu.weight")))
	assert.Equal(t, "100", readFile(filepath.Join(wkDir, "cpu.weight")))
	// 2 cores * 80% * 100000us = 160000 100000
	assert.Equal(t, "160000 100000", readFile(filepath.Join(wkDir, "cpu.max")))
	// memory_reserve_bytes / 2 = 64 MiB
	assert.Equal(t, "67108864", readFile(filepath.Join(sysDir, "memory.low")))
	// 1 GiB - 128 MiB = 896 MiB = 939524096
	assert.Equal(t, "939524096", readFile(filepath.Join(wkDir, "memory.max")))
	// 1024 - 128 = 896
	assert.Equal(t, "896", readFile(filepath.Join(wkDir, "pids.max")))

	scope, ok := cgroupfs.GetSandboxV2Scope()
	require.True(t, ok)
	assert.Equal(t, baseDir, scope.Mount)
	assert.Equal(t, "/", scope.BaseRelPath)
	assert.Equal(t, sysDir, scope.SystemDir)
	assert.Equal(t, wkDir, scope.WorkloadDir)

	cmd := exec.Command("/bin/true")
	appliedFD, fallback := cgroupQoS.preparePlacement(cmd, false)
	assert.True(t, appliedFD)
	assert.False(t, fallback)
	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.UseCgroupFD)
	assert.GreaterOrEqual(t, cmd.SysProcAttr.CgroupFD, 0)

	skippedCmd := exec.Command("/bin/true")
	appliedFD, fallback = cgroupQoS.preparePlacement(skippedCmd, true)
	assert.False(t, appliedFD)
	assert.False(t, fallback)
	assert.Nil(t, skippedCmd.SysProcAttr)
}

func TestInitCgroupQoS_AutoMount(t *testing.T) {
	baseDir := setupFakeCgroupV2Env(t, "cpu memory pids")
	cgroupQoSScanV2Mount = func() (string, error) { return "", errors.New("not mounted") }
	var mountedTarget string
	cgroupQoSMount = func(target string) error {
		mountedTarget = target
		return nil
	}

	cfg := isolation.DefaultConfig()
	cfg.QoS.Cgroup.Enabled = true
	cfg.QoS.Cgroup.AutoMount = true

	require.NoError(t, InitCgroupQoS(cfg, false))
	assert.Equal(t, baseDir, mountedTarget)
	assert.True(t, cgroupQoS.enabled.Load())
	st := cgroupQoS.layerState.Load()
	require.NotNil(t, st)
	assert.Equal(t, "active", st.State)
	assert.Contains(t, st.Message, "auto_mounted=true")
}

func TestInitCgroupQoS_PartialControllersAndClone3Fallback(t *testing.T) {
	baseDir := setupFakeCgroupV2Env(t, "cpu")

	cfg := isolation.DefaultConfig()
	cfg.QoS.Cgroup.Enabled = true

	require.NoError(t, InitCgroupQoS(cfg, true))
	assert.True(t, cgroupQoS.enabled.Load())
	assert.True(t, cgroupQoS.useProcsFallback.Load())

	st := cgroupQoS.layerState.Load()
	require.NotNil(t, st)
	assert.Equal(t, "degraded", st.State)
	assert.Contains(t, st.Message, "clone3compat")

	cmd := exec.Command("/bin/true")
	appliedFD, fallback := cgroupQoS.preparePlacement(cmd, false)
	assert.False(t, appliedFD)
	assert.True(t, fallback)

	cgroupQoS.movePIDToWorkload(9999)
	b, err := os.ReadFile(filepath.Join(baseDir, cgroupWorkloadDirName, "cgroup.procs"))
	require.NoError(t, err)
	assert.Equal(t, "9999", string(b))
}

func TestInitCgroupQoS_AdaptiveSmallMemoryReserve(t *testing.T) {
	baseDir := setupFakeCgroupV2Env(t, "memory")
	// 256 MiB sandbox -> clamp(256/8=32MiB, 32MiB, 128MiB) = 32 MiB reserve
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "memory.max"), []byte("268435456\n"), 0o644))

	cfg := isolation.DefaultConfig()
	cfg.QoS.Cgroup.Enabled = true
	require.NoError(t, InitCgroupQoS(cfg, false))

	sysLow, err := os.ReadFile(filepath.Join(baseDir, cgroupSystemDirName, "memory.low"))
	require.NoError(t, err)
	assert.Equal(t, "16777216", string(sysLow)) // 16 MiB

	wkMax, err := os.ReadFile(filepath.Join(baseDir, cgroupWorkloadDirName, "memory.max"))
	require.NoError(t, err)
	assert.Equal(t, "234881024", string(wkMax)) // 224 MiB
}
