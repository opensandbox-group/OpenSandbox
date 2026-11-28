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
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/cgroupfs"
	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	CgroupProbeArg = "--cgroup-probe"
	CgroupWaitArg  = "--cgroup-wait"

	cgroupSystemDirName     = "opensandbox-system"
	cgroupWorkloadDirName   = "opensandbox-workload"
	cgroupExternalDirName   = "opensandbox-external"
	cgroupMaxValue          = "max"
	cgroupSystemCPUWeight   = 1000
	cgroupWorkloadCPUWeight = 100
	cgroupMinMemoryReserve  = 32 * 1024 * 1024  // 32 MiB
	cgroupMaxMemoryReserve  = 128 * 1024 * 1024 // 128 MiB
	cgroupDefaultPIDReserve = 128
	cgroupCPUPeriodUSec     = 100000
)

// Test-overridable OS seams.
var (
	cgroupQoSDefaultMount    = "/sys/fs/cgroup"
	cgroupQoSProcCgroup      = "/proc/self/cgroup"
	cgroupQoSProcMountinfo   = "/proc/self/mountinfo"
	cgroupQoSProcFilesystems = "/proc/filesystems"
	cgroupQoSProcMeminfo     = "/proc/meminfo"
	cgroupQoSScanV2Mount     = func() (string, error) {
		return cgroupfs.ScanMounts(func(_, fstype string, _ []string) bool {
			return fstype == "cgroup2"
		})
	}
	cgroupQoSMount = func(target string) error {
		return unix.Mount("none", target, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	}
	cgroupQoSProbeCloneIntoCgroup = probeCloneIntoCgroup
	cgroupQoSSelfExe              = os.Executable
	cgroupQoSNumCPU               = runtime.NumCPU
	cgroupQoSSelfPID              = os.Getpid
)

type cgroupQoSState struct {
	mu                sync.Mutex
	enabled           atomic.Bool
	entrypoint        string
	workloadDir       string
	workloadProcsPath string
	workloadFD        int
	useProcsFallback  atomic.Bool
	layerState        atomic.Pointer[LayerState]
}

var cgroupQoS = cgroupQoSState{workloadFD: -1}

// RunCgroupWaitHelper implements the pre-exec stop/ack barrier for the
// cgroup.procs fallback path: it blocks on the inherited ack pipe until the
// parent moves this PID into opensandbox-workload, then execs the target.
func RunCgroupWaitHelper(args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return fmt.Errorf("invalid %s arguments", CgroupWaitArg)
	}
	ackFD, err := strconv.Atoi(args[0])
	if err != nil || ackFD < 3 {
		return fmt.Errorf("invalid ack fd %q", args[0])
	}
	var buf [1]byte
	for {
		n, readErr := unix.Read(ackFD, buf[:])
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		_ = unix.Close(ackFD)
		if readErr != nil {
			return fmt.Errorf("cgroup wait barrier read failed: %w", readErr)
		}
		if n == 0 {
			return errors.New("cgroup wait barrier closed before ack")
		}
		break
	}
	targetArgs := args[2:]
	targetPath, err := exec.LookPath(targetArgs[0])
	if err != nil {
		targetPath = targetArgs[0]
	}
	return syscall.Exec(targetPath, targetArgs, os.Environ())
}

// InitCgroupQoS initializes OSEP-0031 Phase 2 cgroup v2 isolation.
// Invalid static config returns an error; missing kernel/mount/controller
// prerequisites degrade fail-open and are reported via ReportHardening.
func InitCgroupQoS(cfg isolation.Config, initMode bool, clone3Compat bool) error {
	if err := cfg.QoS.Validate(); err != nil {
		return err
	}
	cgroupQoS.reset()

	cc := cfg.QoS.Cgroup
	if !cc.Enabled {
		cgroupQoS.setState(LayerState{
			State:   layerStateDisabled,
			Message: "cgroup v2 QoS isolation is not enabled ([qos.cgroup] enabled = false)",
		})
		return nil
	}

	cgroupQoS.mu.Lock()
	cgroupQoS.entrypoint = cfg.QoS.Entrypoint
	cgroupQoS.mu.Unlock()

	mount, autoMounted, err := ensureCgroupV2Mount(cc.AutoMount)
	if err != nil {
		log.Warn("qos.cgroup: unavailable (continuing without cgroup isolation): %v", err)
		cgroupQoS.setState(LayerState{State: "unsupported", Message: err.Error()})
		return nil
	}

	baseRel, baseDir, err := resolveBaseCgroupV2Dir(mount)
	if err != nil {
		log.Warn("qos.cgroup: base cgroup unavailable (continuing without cgroup isolation): %v", err)
		cgroupQoS.setState(LayerState{State: "unsupported", Message: err.Error()})
		return nil
	}

	systemDir := filepath.Join(baseDir, cgroupSystemDirName)
	workloadDir := filepath.Join(baseDir, cgroupWorkloadDirName)
	externalDir := ""
	if !initMode {
		externalDir = filepath.Join(baseDir, cgroupExternalDirName)
	}

	// Satisfy cgroup v2's no-internal-process rule for both root and delegated
	// non-root bases: move execd into opensandbox-system (and pre-existing
	// external processes into opensandbox-external when init mode is off)
	// before enabling controllers in <base>/cgroup.subtree_control.
	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		msg := fmt.Sprintf("cannot create %s: %v", systemDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "unsupported", Message: msg})
		return nil
	}
	if externalDir != "" {
		if err := os.MkdirAll(externalDir, 0o755); err != nil {
			msg := fmt.Sprintf("cannot create %s: %v", externalDir, err)
			log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
			cgroupQoS.setState(LayerState{State: "degraded", Message: msg})
			return nil
		}
	}
	if err := moveExistingBaseProcs(baseDir, systemDir, externalDir); err != nil {
		msg := fmt.Sprintf("cannot move execd into %s: %v", systemDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "degraded", Message: msg})
		return nil
	}

	activeCtrls, warnings := enableSubtreeControllers(baseDir, []string{"cpu", "memory", "pids"})

	if err := os.MkdirAll(workloadDir, 0o755); err != nil {
		msg := fmt.Sprintf("cannot create %s: %v", workloadDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "degraded", Message: msg})
		return nil
	}

	// Register the sandbox scope only after workloadDir exists so consumers
	// (resources, OOM watcher, eBPF) never observe a dangling path.
	cgroupfs.SetSandboxV2Scope(cgroupfs.SandboxV2Scope{
		Mount:       mount,
		BaseRelPath: baseRel,
		BaseDir:     baseDir,
		SystemDir:   systemDir,
		WorkloadDir: workloadDir,
		ExternalDir: externalDir,
	})

	if slices.Contains(activeCtrls, "cpu") {
		warnings = append(warnings, applyCPULimits(baseDir, systemDir, workloadDir, cc)...)
	}
	if slices.Contains(activeCtrls, "memory") {
		warnings = append(warnings, applyMemoryLimits(baseDir, systemDir, workloadDir, cc)...)
	}
	if slices.Contains(activeCtrls, "pids") {
		warnings = append(warnings, applyPIDsLimits(baseDir, workloadDir, cc)...)
	}

	fd, fdErr := unix.Open(workloadDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if fdErr != nil {
		warnings = append(warnings, fmt.Sprintf("open workload dir fd: %v (using pre-exec cgroup.procs fallback)", fdErr))
		fd = -1
	}

	useFallback := clone3Compat || fd < 0
	if clone3Compat {
		warnings = append(warnings, "clone3compat active (using pre-exec cgroup.procs fallback)")
	} else if fd >= 0 {
		if probeErr := cgroupQoSProbeCloneIntoCgroup(fd); probeErr != nil {
			useFallback = true
			warnings = append(warnings, fmt.Sprintf("CLONE_INTO_CGROUP unavailable (%v); using pre-exec cgroup.procs fallback", probeErr))
		}
	}

	cgroupQoS.mu.Lock()
	cgroupQoS.workloadDir = workloadDir
	cgroupQoS.workloadProcsPath = filepath.Join(workloadDir, "cgroup.procs")
	cgroupQoS.workloadFD = fd
	cgroupQoS.mu.Unlock()
	cgroupQoS.useProcsFallback.Store(useFallback)
	cgroupQoS.enabled.Store(true)

	state := "active"
	if len(activeCtrls) == 0 || len(warnings) > 0 {
		state = "degraded"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("controllers=%v", activeCtrls))
	if autoMounted {
		parts = append(parts, "auto_mounted=true")
	}
	if externalDir != "" {
		parts = append(parts, "external_leaf="+cgroupExternalDirName)
	}
	if len(warnings) > 0 {
		parts = append(parts, strings.Join(warnings, "; "))
	}
	msg := "cgroup v2 QoS " + state + " (" + strings.Join(parts, ", ") + ")"
	if state == "active" {
		log.Info("qos.cgroup: %s", msg)
	} else {
		log.Warn("qos.cgroup: %s", msg)
	}
	cgroupQoS.setState(LayerState{State: state, Message: msg})
	return nil
}

func probeCloneIntoCgroup(workloadFD int) error {
	selfExe, err := cgroupQoSSelfExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(selfExe, CgroupProbeArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    workloadFD,
	}
	err = cmd.Run()
	if err != nil && (errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM)) {
		return err
	}
	return nil
}

func (s *cgroupQoSState) reset() {
	s.enabled.Store(false)
	s.useProcsFallback.Store(false)
	s.mu.Lock()
	if s.workloadFD >= 0 {
		_ = unix.Close(s.workloadFD)
		s.workloadFD = -1
	}
	s.entrypoint = ""
	s.workloadDir = ""
	s.workloadProcsPath = ""
	s.mu.Unlock()
	cgroupfs.ResetSandboxV2Scope()
}

func (s *cgroupQoSState) setState(st LayerState) {
	s.layerState.Store(&st)
}

func (s *cgroupQoSState) isHighEntrypoint() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entrypoint == "high"
}

func (s *cgroupQoSState) preparePlacement(cmd *exec.Cmd, skip bool) (appliedFD bool, needFallback bool) {
	if skip || !s.enabled.Load() {
		return false, false
	}
	if s.useProcsFallback.Load() {
		return false, true
	}
	s.mu.Lock()
	fd := s.workloadFD
	s.mu.Unlock()
	if fd < 0 {
		return false, true
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = fd
	return true, false
}

func (s *cgroupQoSState) startWithPlacement(cmd *exec.Cmd, startFn func() error, skip bool) error {
	_, needFallback := s.preparePlacement(cmd, skip)
	if !needFallback {
		return startFn()
	}
	return s.startWithPreExecBarrier(cmd, startFn)
}

func (s *cgroupQoSState) startWithPreExecBarrier(cmd *exec.Cmd, startFn func() error) error {
	selfExe, exeErr := cgroupQoSSelfExe()
	ackR, ackW, pipeErr := os.Pipe()
	if exeErr != nil || pipeErr != nil {
		if ackR != nil {
			_ = ackR.Close()
		}
		if ackW != nil {
			_ = ackW.Close()
		}
		err := startFn()
		if err == nil && cmd.Process != nil {
			s.movePIDToWorkload(cmd.Process.Pid)
		}
		return err
	}
	defer ackW.Close()

	origPath := cmd.Path
	origArgs := cmd.Args
	if len(origArgs) == 0 {
		origArgs = []string{origPath}
	} else {
		origArgs = append([]string{origPath}, origArgs[1:]...)
	}

	ackFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, ackR)
	cmd.Path = selfExe
	cmd.Args = append([]string{selfExe, CgroupWaitArg, strconv.Itoa(ackFD), "--"}, origArgs...)

	err := startFn()
	_ = ackR.Close()
	if err != nil {
		return err
	}
	if cmd.Process != nil {
		s.movePIDToWorkload(cmd.Process.Pid)
	}
	_, _ = ackW.Write([]byte{1})
	return nil
}

func (s *cgroupQoSState) movePIDToWorkload(pid int) {
	s.mu.Lock()
	procsPath := s.workloadProcsPath
	s.mu.Unlock()
	if procsPath == "" || pid <= 0 {
		return
	}
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		log.Warn("qos.cgroup: move pid %d to %s failed: %v", pid, procsPath, err)
	}
}

func ensureCgroupV2Mount(autoMount bool) (mount string, autoMounted bool, err error) {
	if m, scanErr := cgroupQoSScanV2Mount(); scanErr == nil && m != "" {
		return m, false, nil
	}
	if !autoMount {
		return "", false, errors.New("cgroup v2 is not mounted and auto_mount is disabled")
	}
	fsData, err := os.ReadFile(cgroupQoSProcFilesystems)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", cgroupQoSProcFilesystems, err)
	}
	if !filesystemsHas(fsData, "cgroup2") {
		return "", false, errors.New("kernel does not advertise cgroup2 in /proc/filesystems")
	}
	if err := os.MkdirAll(cgroupQoSDefaultMount, 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", cgroupQoSDefaultMount, err)
	}
	if err := cgroupQoSMount(cgroupQoSDefaultMount); err != nil {
		return "", false, fmt.Errorf("mount cgroup2 at %s: %w", cgroupQoSDefaultMount, err)
	}
	return cgroupQoSDefaultMount, true, nil
}

func filesystemsHas(data []byte, target string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == target {
			return true
		}
	}
	return false
}

func resolveBaseCgroupV2Dir(mount string) (rel string, dir string, err error) {
	data, err := os.ReadFile(cgroupQoSProcCgroup)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", cgroupQoSProcCgroup, err)
	}
	entries := cgroupfs.ParseEntries(data)
	rel = cgroupfs.V2RelPath(entries)
	if rel == "" {
		return "", "", errors.New("no pure cgroup v2 hierarchy in /proc/self/cgroup")
	}
	mountinfo, _ := os.ReadFile(cgroupQoSProcMountinfo)
	dir = cgroupfs.Dir(mountinfo, mount, rel)
	if _, err := os.Stat(filepath.Join(dir, "cgroup.controllers")); err != nil {
		return "", "", fmt.Errorf("cgroup v2 base %s not accessible: %w", dir, err)
	}
	return rel, dir, nil
}

func moveExistingBaseProcs(baseDir, systemDir, externalDir string) error {
	systemProcs := filepath.Join(systemDir, "cgroup.procs")
	selfPID := strconv.Itoa(cgroupQoSSelfPID())
	if err := os.WriteFile(systemProcs, []byte(selfPID), 0o600); err != nil {
		return err
	}
	// In delegated non-root cgroups (e.g. Docker --cgroupns=private), cgroup v2's
	// no-internal-process rule rejects subtree_control writes with EBUSY if any
	// startup process remains in <base>. When EXECD_INIT is off, move other
	// pre-existing processes (such as the external bootstrap ENTRYPOINT) into a
	// neutral external leaf rather than opensandbox-system.
	otherTargetDir := systemDir
	if externalDir != "" {
		otherTargetDir = externalDir
	}
	otherProcs := filepath.Join(otherTargetDir, "cgroup.procs")
	if data, readErr := os.ReadFile(filepath.Join(baseDir, "cgroup.procs")); readErr == nil {
		for _, field := range strings.Fields(string(data)) {
			if field == selfPID {
				continue
			}
			if err := os.WriteFile(otherProcs, []byte(field), 0o600); err != nil {
				log.Warn("qos.cgroup: move existing pid %s to %s failed: %v", field, otherTargetDir, err)
			}
		}
	}
	return nil
}

func enableSubtreeControllers(baseDir string, want []string) (active []string, warnings []string) {
	data, err := os.ReadFile(filepath.Join(baseDir, "cgroup.controllers"))
	if err != nil {
		return nil, []string{fmt.Sprintf("read cgroup.controllers: %v", err)}
	}
	avail := strings.Fields(string(data))
	subtreePath := filepath.Join(baseDir, "cgroup.subtree_control")
	for _, ctrl := range want {
		if !slices.Contains(avail, ctrl) {
			warnings = append(warnings, fmt.Sprintf("controller %q unavailable", ctrl))
			continue
		}
		if err := os.WriteFile(subtreePath, []byte("+"+ctrl), 0o600); err != nil {
			warnings = append(warnings, fmt.Sprintf("enable +%s failed: %v", ctrl, err))
			continue
		}
		active = append(active, ctrl)
	}
	return active, warnings
}

func applyCPULimits(baseDir, systemDir, workloadDir string, cfg isolation.CgroupQoSConfig) []string {
	var warnings []string
	if err := os.WriteFile(filepath.Join(systemDir, "cpu.weight"), []byte(strconv.Itoa(cgroupSystemCPUWeight)), 0o600); err != nil {
		warnings = append(warnings, fmt.Sprintf("system cpu.weight: %v", err))
	}
	if err := os.WriteFile(filepath.Join(workloadDir, "cpu.weight"), []byte(strconv.Itoa(cgroupWorkloadCPUWeight)), 0o600); err != nil {
		warnings = append(warnings, fmt.Sprintf("workload cpu.weight: %v", err))
	}
	if cfg.WorkloadCPUMaxPercent > 0 {
		cores := effectiveCPUCores(baseDir)
		quota := int64(math.Round(cores * float64(cfg.WorkloadCPUMaxPercent) / 100.0 * cgroupCPUPeriodUSec))
		if quota < 1000 {
			quota = 1000
		}
		val := fmt.Sprintf("%d %d", quota, cgroupCPUPeriodUSec)
		if err := os.WriteFile(filepath.Join(workloadDir, "cpu.max"), []byte(val), 0o600); err != nil {
			warnings = append(warnings, fmt.Sprintf("workload cpu.max: %v", err))
		}
	}
	return warnings
}

func effectiveCPUCores(baseDir string) float64 {
	if data, err := os.ReadFile(filepath.Join(baseDir, "cpu.max")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 && fields[0] != cgroupMaxValue {
			quota, qErr := strconv.ParseInt(fields[0], 10, 64)
			period, pErr := strconv.ParseInt(fields[1], 10, 64)
			if qErr == nil && pErr == nil && quota > 0 && period > 0 {
				return float64(quota) / float64(period)
			}
		}
	}
	n := cgroupQoSNumCPU()
	if n < 1 {
		n = 1
	}
	return float64(n)
}

func applyMemoryLimits(baseDir, systemDir, workloadDir string, cfg isolation.CgroupQoSConfig) []string {
	var warnings []string
	total, ok := effectiveTotalMemoryBytes(baseDir)
	if !ok {
		return []string{"workload memory.max skipped: unable to determine total memory"}
	}
	reserve := cfg.MemoryReserveBytes
	if reserve == 0 {
		reserve = total / 8
		if reserve < cgroupMinMemoryReserve {
			reserve = cgroupMinMemoryReserve
		}
		if reserve > cgroupMaxMemoryReserve {
			reserve = cgroupMaxMemoryReserve
		}
	}
	if total <= reserve {
		return []string{fmt.Sprintf("workload memory.max skipped: total (%d) <= reserve (%d)", total, reserve)}
	}
	if systemLow := reserve / 2; systemLow > 0 {
		if err := os.WriteFile(filepath.Join(systemDir, "memory.low"), []byte(strconv.FormatInt(systemLow, 10)), 0o600); err != nil {
			warnings = append(warnings, fmt.Sprintf("system memory.low: %v", err))
		}
	}
	workloadMax := total - reserve
	if err := os.WriteFile(filepath.Join(workloadDir, "memory.max"), []byte(strconv.FormatInt(workloadMax, 10)), 0o600); err != nil {
		warnings = append(warnings, fmt.Sprintf("workload memory.max: %v", err))
	}
	return warnings
}

func effectiveTotalMemoryBytes(baseDir string) (int64, bool) {
	if data, err := os.ReadFile(filepath.Join(baseDir, "memory.max")); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != cgroupMaxValue {
			if v, parseErr := strconv.ParseInt(s, 10, 64); parseErr == nil && v > 0 {
				return v, true
			}
		}
	}
	data, err := os.ReadFile(cgroupQoSProcMeminfo)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil && kb > 0 {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

func applyPIDsLimits(baseDir, workloadDir string, cfg isolation.CgroupQoSConfig) []string {
	var warnings []string
	workloadPIDs := cfg.WorkloadPIDsMax
	if workloadPIDs == 0 {
		data, err := os.ReadFile(filepath.Join(baseDir, "pids.max"))
		if err != nil {
			return []string{fmt.Sprintf("pids: read base pids.max failed (%v), no workload PID limit set", err)}
		}
		s := strings.TrimSpace(string(data))
		if s == "" || s == cgroupMaxValue {
			return []string{"pids: base pids.max is unbounded, no workload PID limit set"}
		}
		if baseMax, parseErr := strconv.ParseInt(s, 10, 64); parseErr == nil && baseMax > cgroupDefaultPIDReserve {
			workloadPIDs = baseMax - cgroupDefaultPIDReserve
		} else {
			return []string{fmt.Sprintf("pids: base pids.max (%s) <= reserve (%d), no workload PID limit set", s, cgroupDefaultPIDReserve)}
		}
	}
	if workloadPIDs > 0 {
		if err := os.WriteFile(filepath.Join(workloadDir, "pids.max"), []byte(strconv.FormatInt(workloadPIDs, 10)), 0o600); err != nil {
			warnings = append(warnings, fmt.Sprintf("workload pids.max: %v", err))
		}
	}
	return warnings
}
