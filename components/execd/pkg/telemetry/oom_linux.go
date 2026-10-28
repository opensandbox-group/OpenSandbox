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

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/cgroupfs"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	procCgroupFile     = "/proc/self/cgroup"
	procDir            = "/proc"
	procStatFile       = "/proc/stat"
	cgroupV2DefaultDir = "/sys/fs/cgroup"
	cgroupV1MemoryDir  = "/sys/fs/cgroup/memory"
	// oomMaxChildCgroups caps how many first-level child cgroups are polled
	// for kill localization per tick.
	oomMaxChildCgroups = 64
	// oomMaxTrackedPids caps how many process detail records the tracker
	// keeps; pids beyond the cap are still observed but get no details, so
	// attribution degrades instead of growing without bound.
	oomMaxTrackedPids = 4096
	// oomMaxPendingExited bounds the exited-process buffer carried between
	// detections.
	oomMaxPendingExited = 64
	// oomMaxKillCgroups caps child cgroups named in one log line.
	oomMaxKillCgroups = 3
	// oomUnlimitedV1Bytes: v1 reports a huge byte value instead of "max".
	oomUnlimitedV1Bytes = 1 << 50
)

const (
	oomSourceV2 = "cgroup-v2"
	oomSourceV1 = "cgroup-v1"
)

// oomSource is the cgroupfs location of the sandbox OOM counters. execd sits
// in the sandbox's own cgroup (often also its own cgroup namespace), so its
// /proc/self/cgroup path resolved under the cgroup mount scopes the whole
// sandbox subtree: kills of user processes and of nested cgroups all show up
// in the counters read here.
type oomSource struct {
	mode       string
	cgroupPath string
	dir        string // cgroup directory holding the memory interface files
	eventsPath string // memory.events (v2) or memory.oom_control (v1)
	localPath  string // memory.events.local (v2 only, empty otherwise)
}

// runOOMWatch polls the sandbox cgroup OOM counters until ctx is done.
func runOOMWatch(ctx context.Context) {
	source, err := newOOMSource()
	if err != nil {
		log.Warn("oom: watch disabled: %v", err)
		return
	}
	watchOOMSource(ctx, source, oomPollInterval)
}

// oomWatchState carries one watcher's polling state across ticks.
type oomWatchState struct {
	source  *oomSource
	tracker *procTracker

	prev         oomSample
	prevChildren map[string]int64
	prevFp       string
	havePrev     bool

	pendingExited []oomExited
	hold          *oomDelta
	holdPrev      oomSample
	holdCur       oomSample

	readErrReported bool
}

// watchOOMSource polls the sandbox cgroup OOM counters until ctx is done.
// The baseline is seeded synchronously so kills during the first interval
// are reported instead of being swallowed.
func watchOOMSource(ctx context.Context, source *oomSource, interval time.Duration) {
	log.Info("oom: watching for OOM kills (source=%s, cgroup=%s, interval=%s)",
		source.mode, source.cgroupPath, interval)

	state := &oomWatchState{source: source, tracker: newProcTracker()}
	state.seed()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		state.tick(ctx)
	}
}

// seed takes the initial baseline without waiting for the first tick.
func (s *oomWatchState) seed() {
	s.prevChildren = s.source.childKillCounts()
	s.prevFp = bindingFingerprint()
	prev, err := s.source.sample()
	if err != nil {
		log.Warn("oom: reading %s failed at startup: %v", s.source.eventsPath, err)
		return
	}
	s.prev, s.havePrev = prev, true
}

// tick runs one polling step. skip is true when the step must not proceed to
// detection (read failure or binding handoff).
func (s *oomWatchState) tick(ctx context.Context) (skip bool) {
	cur, err := s.source.sample()
	if err != nil {
		// Report once: repeated failures (e.g. cgroupfs gone during
		// teardown) would otherwise spam the log every interval.
		if !s.readErrReported {
			s.readErrReported = true
			log.Warn("oom: reading %s failed, retrying: %v", s.source.eventsPath, err)
		}
		return true
	}
	s.readErrReported = false

	// Cheap every-tick pass: directory read + stats of new pids only. Exited
	// candidates are held in a bounded buffer across ticks.
	s.pendingExited = append(s.pendingExited, s.tracker.observe()...)
	if len(s.pendingExited) > oomMaxPendingExited {
		s.pendingExited = s.pendingExited[len(s.pendingExited)-oomMaxPendingExited:]
	}
	curChildren := s.source.childKillCounts()

	if s.handleRebind(cur, curChildren) {
		return true
	}
	s.emitHeld(ctx, curChildren)

	if s.havePrev {
		if delta := diffOOMSample(s.prev, cur); delta.total > 0 {
			s.holdDetection(delta, s.prev, cur)
		}
	}
	s.prev, s.prevChildren, s.havePrev = cur, curChildren, true
	return false
}

// handleRebind drops the in-flight interval when the RuntimeBinding changed:
// a /internal/init handoff switches the attribution to the new sandbox while
// the cgroup counters stay continuous, so exporting the old sandbox's kills
// under the new identity must be avoided.
func (s *oomWatchState) handleRebind(cur oomSample, curChildren map[string]int64) bool {
	fp := bindingFingerprint()
	if fp == s.prevFp {
		return false
	}
	if s.havePrev {
		log.Info("oom: binding handoff detected, dropping the in-flight interval")
	}
	s.hold, s.holdPrev, s.holdCur, s.pendingExited = nil, oomSample{}, oomSample{}, nil
	s.prev, s.prevChildren, s.prevFp, s.havePrev = cur, curChildren, fp, true
	return true
}

// emitHeld reports a detection held since the previous tick.
func (s *oomWatchState) emitHeld(ctx context.Context, curChildren map[string]int64) {
	if !s.havePrev || s.hold == nil {
		return
	}
	emitOOMKill(ctx, s.source.eventSource(),
		s.source.killContext(s.holdPrev, s.holdCur, s.prevChildren, curChildren),
		*s.hold, limitExited(s.pendingExited, int(s.hold.total)),
		s.tracker.bootTime, time.Now().Unix())
	s.pendingExited, s.hold = nil, nil
}

// holdDetection folds a new kill delta into the pending hold.
func (s *oomWatchState) holdDetection(delta oomDelta, prev, cur oomSample) {
	if s.hold == nil {
		copied := delta
		s.hold = &copied
		s.holdPrev, s.holdCur = prev, cur
		return
	}
	s.hold.total += delta.total
	if delta.haveLocal {
		s.hold.haveLocal = true
		s.hold.local += delta.local
		s.hold.descendant += delta.descendant
	}
	s.holdCur = cur
}

// killContext assembles the event context for the log line: where the memory
// stood, which OOM sub-events fired, and which child cgroup subtrees the
// kills landed in (hierarchical child counters localize kills at any depth;
// they cover any nested cgroup inside the sandbox, session-created or not).
func (s *oomSource) killContext(prev, cur oomSample, prevChildren, curChildren map[string]int64) string {
	var parts []string
	if mem := s.memoryContext(); mem != "" {
		parts = append(parts, mem)
	}
	if events := formatEventsDelta(prev, cur); events != "" {
		parts = append(parts, events)
	}
	kills := killCgroupNames(prevChildren, curChildren)
	if len(kills) > 0 {
		shown := kills
		if len(shown) > oomMaxKillCgroups {
			shown = append(shown[:oomMaxKillCgroups], fmt.Sprintf("+%d more", len(kills)-oomMaxKillCgroups))
		}
		parts = append(parts, "kill_cgroups="+strings.Join(shown, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func newOOMSource() (*oomSource, error) {
	if source, err := newCgroupV2Source(); err == nil {
		return source, nil
	}
	if source, err := newCgroupV1Source(); err == nil {
		return source, nil
	}
	return nil, errors.New("no cgroup v2 or v1 memory events interface visible")
}

func newCgroupV2Source() (*oomSource, error) {
	cgroupPath, err := cgroupV2RelPath()
	if err != nil {
		return nil, err
	}
	mount, err := cgroupV2Mount()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(mount, filepath.FromSlash(cgroupPath))
	eventsPath := filepath.Join(dir, "memory.events")
	if _, err := os.Stat(eventsPath); err != nil {
		return nil, fmt.Errorf("stat %s: %w", eventsPath, err)
	}
	source := &oomSource{
		mode:       oomSourceV2,
		cgroupPath: cgroupPath,
		dir:        dir,
		eventsPath: eventsPath,
	}
	// memory.events.local separates kills of processes in this cgroup from
	// kills propagated from descendants; absent on very old kernels.
	localPath := filepath.Join(dir, "memory.events.local")
	if _, err := os.Stat(localPath); err == nil {
		source.localPath = localPath
	}
	return source, nil
}

func newCgroupV1Source() (*oomSource, error) {
	cgroupPath, err := cgroupV1MemoryRelPath()
	if err != nil {
		return nil, err
	}
	mount, err := cgroupV1MemoryMount()
	if err != nil {
		return nil, err
	}
	// v1 counters are per-cgroup: this is best-effort and does not cover
	// descendant cgroups the way the v2 hierarchical counters do.
	dir := filepath.Join(mount, filepath.FromSlash(cgroupPath))
	oomControl := filepath.Join(dir, "memory.oom_control")
	if _, err := os.Stat(oomControl); err != nil {
		return nil, fmt.Errorf("stat %s: %w", oomControl, err)
	}
	return &oomSource{
		mode:       oomSourceV1,
		cgroupPath: cgroupPath,
		dir:        dir,
		eventsPath: oomControl,
	}, nil
}

func (s *oomSource) eventSource() oomEventSource {
	return oomEventSource{Mode: s.mode, CgroupPath: s.cgroupPath}
}

func (s *oomSource) sample() (oomSample, error) {
	data, err := os.ReadFile(s.eventsPath)
	if err != nil {
		return oomSample{}, err
	}
	counters := parseOOMEvents(data)
	if _, ok := counters["oom_kill"]; !ok {
		return oomSample{}, fmt.Errorf("no oom_kill counter in %s", s.eventsPath)
	}
	sample := oomSample{counters: counters}
	if s.localPath != "" {
		data, err := os.ReadFile(s.localPath)
		if err != nil {
			return oomSample{}, err
		}
		local := parseOOMEvents(data)
		localKills, ok := local["oom_kill"]
		if !ok {
			return oomSample{}, fmt.Errorf("no oom_kill counter in %s", s.localPath)
		}
		sample.local = localKills
		sample.haveLocal = true
	}
	return sample, nil
}

// memoryContext describes how close the watched cgroup was to its limit when
// the detection fired. Best-effort: unreadable files are skipped.
func (s *oomSource) memoryContext() string {
	if s.mode == oomSourceV2 {
		current, ok := readMemoryBytes(filepath.Join(s.dir, "memory.current"))
		if !ok {
			return ""
		}
		_, limitStr := readMemoryLimit(filepath.Join(s.dir, "memory.max"))
		context := fmt.Sprintf("memory=%s/%s", humanBytes(current), limitStr)
		if peak, ok := readMemoryBytes(filepath.Join(s.dir, "memory.peak")); ok {
			context += " peak=" + humanBytes(peak)
		}
		return context
	}
	// cgroup v1 best-effort.
	current, ok1 := readMemoryBytes(filepath.Join(s.dir, "memory.usage_in_bytes"))
	limit, ok2 := readMemoryBytes(filepath.Join(s.dir, "memory.limit_in_bytes"))
	if !ok1 || !ok2 {
		return ""
	}
	limitStr := humanBytes(limit)
	if limit >= oomUnlimitedV1Bytes {
		limitStr = "unlimited"
	}
	return fmt.Sprintf("memory=%s/%s", humanBytes(current), limitStr)
}

// childKillCounts returns the hierarchical oom_kill counter of every
// first-level child cgroup. Hierarchical (not .local) counters are used so a
// kill nested deeper than one level still increments the child subtree that
// contains it.
func (s *oomSource) childKillCounts() map[string]int64 {
	if s.mode != oomSourceV2 {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	counts := make(map[string]int64)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(counts) >= oomMaxChildCgroups {
			break
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name(), "memory.events"))
		if err != nil {
			continue
		}
		if kills, ok := parseOOMEvents(data)["oom_kill"]; ok {
			counts[entry.Name()] = kills
		}
	}
	return counts
}

// readMemoryBytes reads a cgroupfs file holding a single byte value.
func readMemoryBytes(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// readMemoryLimit reads memory.max: a byte value or the literal "max"
// (unlimited).
func readMemoryLimit(path string) (int64, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	text := strings.TrimSpace(string(data))
	if text == "max" {
		return 0, "unlimited"
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, ""
	}
	return value, humanBytes(value)
}

// cgroupV2RelPath returns the sandbox's cgroup v2 path relative to the
// hierarchy root, from the "0::<path>" line of /proc/self/cgroup.
func cgroupV2RelPath() (string, error) {
	data, err := os.ReadFile(procCgroupFile)
	if err != nil {
		return "", err
	}
	entries := cgroupfs.ParseEntries(data)
	if v2Path := cgroupfs.V2RelPath(entries); v2Path != "" {
		return v2Path, nil
	}
	if cgroupfs.V1RelPath(entries, "memory") != "" {
		// Hybrid host (v1 controllers + unified hierarchy): the "0::" entry
		// is degenerate — its root is not this process's memory cgroup, and
		// reading counters there would silently attribute unrelated host
		// kills to the sandbox. Memory accounting lives on the v1 hierarchy;
		// refuse v2 and let the v1 fallback take over.
		return "", errors.New("hybrid cgroup host: memory is on the v1 hierarchy")
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// cgroupV2Mount locates the cgroup v2 filesystem mount.
func cgroupV2Mount() (string, error) {
	if _, err := os.Stat(filepath.Join(cgroupV2DefaultDir, "cgroup.controllers")); err == nil {
		return cgroupV2DefaultDir, nil
	}
	return cgroupfs.ScanMounts(func(mountPoint, fstype string, options []string) bool {
		return fstype == "cgroup2"
	})
}

// cgroupV1MemoryRelPath returns the sandbox's path in the v1 memory
// hierarchy, from /proc/self/cgroup.
func cgroupV1MemoryRelPath() (string, error) {
	data, err := os.ReadFile(procCgroupFile)
	if err != nil {
		return "", err
	}
	if v1MemoryPath := cgroupfs.V1RelPath(cgroupfs.ParseEntries(data), "memory"); v1MemoryPath != "" {
		return v1MemoryPath, nil
	}
	return "", errors.New("no memory hierarchy entry in /proc/self/cgroup")
}

// cgroupV1MemoryMount locates the v1 memory controller mount.
func cgroupV1MemoryMount() (string, error) {
	if _, err := os.Stat(cgroupV1MemoryDir); err == nil {
		return cgroupV1MemoryDir, nil
	}
	return cgroupfs.ScanMounts(func(mountPoint, fstype string, options []string) bool {
		return fstype == "cgroup" && cgroupfs.OptionsHave(options, "memory")
	})
}

// procTracker observes /proc incrementally: every tick costs one directory
// read, plus one stat read per newly spawned pid. When a pid disappears, its
// recorded details become an exited-process candidate. Details are captured
// once when a pid is first observed (RSS of long-lived processes can be
// stale), which keeps a calm sandbox with a stable process table near zero
// cost per tick.
type procTracker struct {
	pidset   map[int]struct{} // pids seen in the previous tick; nil until the first observe
	details  map[int]oomProc  // details captured when a pid was first observed
	bootTime int64            // kernel boot time (epoch seconds), read once
}

func newProcTracker() *procTracker {
	tracker := &procTracker{details: map[int]oomProc{}}
	if data, err := os.ReadFile(procStatFile); err == nil {
		tracker.bootTime = bootTimeFromProcStat(data)
	}
	return tracker
}

// observe scans /proc once and returns the processes that vanished since the
// previous tick, ordered by pid. A failed scan skips vanish detection for
// that tick instead of reporting against an unknown view.
func (t *procTracker) observe() []oomExited {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	cur := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cur[pid] = struct{}{}
		if _, known := t.details[pid]; !known && len(t.details) < oomMaxTrackedPids {
			// Capture details while the process is alive; after it exits
			// there is nothing left to read.
			if data, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "stat")); err == nil {
				if proc, ok := parseStat(data); ok {
					t.details[pid] = proc
				}
			}
		}
	}

	var exited []oomExited
	if t.pidset != nil {
		for pid := range t.pidset {
			if _, alive := cur[pid]; alive {
				continue
			}
			proc, known := t.details[pid]
			delete(t.details, pid)
			if !known {
				// Never observed alive (e.g. tracker cap): reporting an
				// empty record would fabricate details, so skip it.
				continue
			}
			exited = append(exited, oomExited{Pid: pid, Proc: proc})
		}
		sort.Slice(exited, func(i, j int) bool { return exited[i].Pid < exited[j].Pid })
	}
	t.pidset = cur
	return exited
}

// bootTimeFromProcStat extracts the btime (boot time, epoch seconds) line
// from /proc/stat.
func bootTimeFromProcStat(data []byte) int64 {
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "btime" {
			if value, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return value
			}
		}
	}
	return 0
}
