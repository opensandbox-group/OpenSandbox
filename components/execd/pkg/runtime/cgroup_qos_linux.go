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
	cgroupSystemDirName     = "opensandbox-system"
	cgroupWorkloadDirName   = "opensandbox-workload"
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
	cgroupQoSNumCPU  = runtime.NumCPU
	cgroupQoSSelfPID = os.Getpid
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

// InitCgroupQoS initializes OSEP-0031 Phase 2 cgroup v2 isolation.
// Invalid static config returns an error; missing kernel/mount/controller
// prerequisites degrade fail-open and are reported via ReportHardening.
func InitCgroupQoS(cfg isolation.Config, clone3Compat bool) error {
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

	// Satisfy cgroup v2's no-internal-process rule for both root and delegated
	// non-root bases: move execd into opensandbox-system before enabling
	// controllers in <base>/cgroup.subtree_control.
	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		msg := fmt.Sprintf("cannot create %s: %v", systemDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "unsupported", Message: msg})
		return nil
	}
	if err := os.WriteFile(filepath.Join(systemDir, "cgroup.procs"), []byte(strconv.Itoa(cgroupQoSSelfPID())), 0o644); err != nil {
		msg := fmt.Sprintf("cannot move execd into %s: %v", systemDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "degraded", Message: msg})
		return nil
	}

	cgroupfs.SetSandboxV2Scope(cgroupfs.SandboxV2Scope{
		Mount:       mount,
		BaseRelPath: baseRel,
		SystemDir:   systemDir,
		WorkloadDir: workloadDir,
	})

	activeCtrls, warnings := enableSubtreeControllers(baseDir, []string{"cpu", "memory", "pids"})

	if err := os.MkdirAll(workloadDir, 0o755); err != nil {
		msg := fmt.Sprintf("cannot create %s: %v", workloadDir, err)
		log.Warn("qos.cgroup: %s (continuing without cgroup isolation)", msg)
		cgroupQoS.setState(LayerState{State: "degraded", Message: msg})
		return nil
	}

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
		warnings = append(warnings, fmt.Sprintf("open workload dir fd: %v (using cgroup.procs fallback)", fdErr))
		fd = -1
	}

	cgroupQoS.mu.Lock()
	cgroupQoS.workloadDir = workloadDir
	cgroupQoS.workloadProcsPath = filepath.Join(workloadDir, "cgroup.procs")
	cgroupQoS.workloadFD = fd
	cgroupQoS.mu.Unlock()

	if clone3Compat || fd < 0 {
		cgroupQoS.useProcsFallback.Store(true)
		if clone3Compat {
			warnings = append(warnings, "clone3compat active (using cgroup.procs fallback)")
		}
	}
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

func (s *cgroupQoSState) markProcsFallback(reason error) {
	if !s.useProcsFallback.Swap(true) {
		log.Warn("qos.cgroup: CLONE_INTO_CGROUP unavailable (%v), falling back to cgroup.procs", reason)
		if cur := s.layerState.Load(); cur != nil {
			s.setState(LayerState{
				State:   "degraded",
				Message: cur.Message + fmt.Sprintf("; CLONE_INTO_CGROUP fallback: %v", reason),
			})
		}
	}
}

func (s *cgroupQoSState) movePIDToWorkload(pid int) {
	s.mu.Lock()
	procsPath := s.workloadProcsPath
	s.mu.Unlock()
	if procsPath == "" || pid <= 0 {
		return
	}
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
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
		if err := os.WriteFile(subtreePath, []byte("+"+ctrl), 0o644); err != nil {
			warnings = append(warnings, fmt.Sprintf("enable +%s failed: %v", ctrl, err))
			continue
		}
		active = append(active, ctrl)
	}
	return active, warnings
}

func applyCPULimits(baseDir, systemDir, workloadDir string, cfg isolation.CgroupQoSConfig) []string {
	var warnings []string
	if err := os.WriteFile(filepath.Join(systemDir, "cpu.weight"), []byte(strconv.Itoa(cgroupSystemCPUWeight)), 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("system cpu.weight: %v", err))
	}
	if err := os.WriteFile(filepath.Join(workloadDir, "cpu.weight"), []byte(strconv.Itoa(cgroupWorkloadCPUWeight)), 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("workload cpu.weight: %v", err))
	}
	if cfg.WorkloadCPUMaxPercent > 0 {
		cores := effectiveCPUCores(baseDir)
		quota := int64(math.Round(cores * float64(cfg.WorkloadCPUMaxPercent) / 100.0 * cgroupCPUPeriodUSec))
		if quota < 1000 {
			quota = 1000
		}
		val := fmt.Sprintf("%d %d", quota, cgroupCPUPeriodUSec)
		if err := os.WriteFile(filepath.Join(workloadDir, "cpu.max"), []byte(val), 0o644); err != nil {
			warnings = append(warnings, fmt.Sprintf("workload cpu.max: %v", err))
		}
	}
	return warnings
}

func effectiveCPUCores(baseDir string) float64 {
	if data, err := os.ReadFile(filepath.Join(baseDir, "cpu.max")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 && fields[0] != "max" {
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
		if err := os.WriteFile(filepath.Join(systemDir, "memory.low"), []byte(strconv.FormatInt(systemLow, 10)), 0o644); err != nil {
			warnings = append(warnings, fmt.Sprintf("system memory.low: %v", err))
		}
	}
	workloadMax := total - reserve
	if err := os.WriteFile(filepath.Join(workloadDir, "memory.max"), []byte(strconv.FormatInt(workloadMax, 10)), 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("workload memory.max: %v", err))
	}
	return warnings
}

func effectiveTotalMemoryBytes(baseDir string) (int64, bool) {
	if data, err := os.ReadFile(filepath.Join(baseDir, "memory.max")); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != "max" {
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
		if data, err := os.ReadFile(filepath.Join(baseDir, "pids.max")); err == nil {
			s := strings.TrimSpace(string(data))
			if s != "" && s != "max" {
				if baseMax, parseErr := strconv.ParseInt(s, 10, 64); parseErr == nil && baseMax > cgroupDefaultPIDReserve {
					workloadPIDs = baseMax - cgroupDefaultPIDReserve
				}
			}
		}
	}
	if workloadPIDs > 0 {
		if err := os.WriteFile(filepath.Join(workloadDir, "pids.max"), []byte(strconv.FormatInt(workloadPIDs, 10)), 0o644); err != nil {
			warnings = append(warnings, fmt.Sprintf("workload pids.max: %v", err))
		}
	}
	return warnings
}
