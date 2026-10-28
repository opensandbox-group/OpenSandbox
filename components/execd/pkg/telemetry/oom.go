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
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	// oomPollInterval balances detection latency against cgroupfs polling
	// cost; kills within one interval collapse into a single counter diff.
	oomPollInterval = 5 * time.Second
	// oomMaxExited caps exited-process entries in one log line.
	oomMaxExited = 5
	// clockTicksPerSecond is the USER_HZ the kernel uses for the starttime
	// field of /proc/<pid>/stat (the same constant gopsutil assumes).
	clockTicksPerSecond = 100
)

// StartOOMWatch launches the sandbox cgroup OOM kill watcher. Every detected
// kill increments execd.system.oom.kills and emits a troubleshooting log
// line. The watcher runs independently of OTLP export so OOM logs work even
// when metrics are disabled. The returned function stops the watcher.
func StartOOMWatch(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	safego.Go(func() { runOOMWatch(ctx) })
	return cancel
}

// oomProc is a process snapshot entry used for exited-process attribution.
type oomProc struct {
	Comm       string
	Ppid       int
	RSSBytes   int64
	StartTicks int64 // starttime field of /proc/<pid>/stat, in clock ticks
}

// oomSample is one read of the cgroup OOM counters. counters carries the
// full memory.events key set (oom, oom_kill, oom_group_kill, ...);
// oom_kill drives detection and is the kill total of the whole watched
// cgroup subtree. local counts kills of processes belonging to the watched
// cgroup itself, without descendants (cgroup v2 only).
type oomSample struct {
	counters  map[string]int64
	local     int64
	haveLocal bool
}

func (s oomSample) killTotal() int64 {
	return s.counters["oom_kill"]
}

// oomDelta is the change between two samples.
type oomDelta struct {
	total      int64
	local      int64
	descendant int64
	haveLocal  bool
}

// oomEventSource identifies where a detection came from, for logging.
type oomEventSource struct {
	Mode       string // e.g. "cgroup-v2"
	CgroupPath string // sandbox cgroup path as seen inside the container
}

// oomExited is a process that disappeared from /proc between the polls
// surrounding an OOM event: a candidate victim, since normal exits are
// indistinguishable from kills at this distance.
type oomExited struct {
	Pid  int
	Proc oomProc
}

// diffOOMSample computes the delta between two counter samples. A shrinking
// counter is treated as a reset and counted from zero.
func diffOOMSample(prev, cur oomSample) oomDelta {
	delta := oomDelta{haveLocal: cur.haveLocal}
	delta.total = monotonicDelta(prev.killTotal(), cur.killTotal())
	if cur.haveLocal {
		delta.local = monotonicDelta(prev.local, cur.local)
		// memory.events and memory.events.local are read separately; a kill
		// landing between the two reads can push local past total.
		if delta.local > delta.total {
			delta.local = delta.total
		}
		delta.descendant = delta.total - delta.local
	}
	return delta
}

func monotonicDelta(prev, cur int64) int64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// parseOOMEvents parses a cgroup memory event file ("<key> <value>" lines:
// memory.events, memory.events.local, memory.oom_control) into a key map.
// Malformed lines are skipped.
func parseOOMEvents(data []byte) map[string]int64 {
	counters := make(map[string]int64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		counters[fields[0]] = value
	}
	return counters
}

// parseStat extracts comm, ppid, RSS, and starttime from /proc/<pid>/stat
// content. comm may contain spaces, so it is taken between the first '(' and
// the last ')'; the remaining fields then start at state (field 3).
func parseStat(data []byte) (oomProc, bool) {
	open := bytes.IndexByte(data, '(')
	closing := bytes.LastIndexByte(data, ')')
	if open < 0 || closing <= open {
		return oomProc{}, false
	}
	proc := oomProc{Comm: string(data[open+1 : closing])}
	fields := strings.Fields(string(data[closing+1:]))
	// Field N of stat (1-based, comm counted) sits at fields[N-3].
	if len(fields) < 22 {
		return oomProc{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return oomProc{}, false
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return oomProc{}, false
	}
	rssPages, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return oomProc{}, false
	}
	proc.Ppid = ppid
	proc.StartTicks = startTicks
	proc.RSSBytes = rssPages * int64(os.Getpagesize())
	return proc, true
}

// victimAge returns how long a process had been alive when the snapshot was
// taken, or -1 when unknown.
func victimAge(bootTime, startTicks, now int64) int64 {
	if bootTime == 0 || startTicks <= 0 {
		return -1
	}
	age := now - bootTime - startTicks/clockTicksPerSecond
	if age < 0 {
		return -1
	}
	return age
}

// bindingFingerprint identifies the RuntimeBinding in effect right now, so
// the watcher can drop counter intervals crossing a /internal/init handoff:
// the cgroup counters are continuous across rebinds while their attribution
// switches to the new sandbox.
func bindingFingerprint() string {
	b := binding.Current()
	if b == nil {
		return ""
	}
	return strconv.FormatUint(b.Generation, 10) + "/" + b.SandboxID
}

// killCgroupNames returns the child cgroups whose subtree received kills in
// the last interval: hierarchical counts that incremented, or newly seen
// children with a positive count. A fresh child with zero kills is not
// blamed just for being new. Names are sorted.
func killCgroupNames(prev, cur map[string]int64) []string {
	var names []string
	for name, count := range cur {
		if count <= 0 {
			continue
		}
		if prevCount, seen := prev[name]; seen && monotonicDelta(prevCount, count) == 0 {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// limitExited keeps the most recent entries up to the limit: freshly exited
// processes are the likelier kill candidates, and listing more exited
// processes than detected kills would assert causality the heuristic cannot
// back up.
func limitExited(exited []oomExited, limit int) []oomExited {
	if limit >= 0 && len(exited) > limit {
		return exited[len(exited)-limit:]
	}
	return exited
}

// formatEventsDelta renders the deltas of the OOM-related memory.events
// counters (cgroup v2 only: the v1 oom_control "oom" key is a state flag,
// not a counter).
func formatEventsDelta(prev, cur oomSample) string {
	if !cur.haveLocal {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, key := range []string{"oom", "oom_kill", "oom_group_kill"} {
		if _, present := cur.counters[key]; !present {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", key, monotonicDelta(prev.counters[key], cur.counters[key])))
	}
	if len(parts) == 0 {
		return ""
	}
	return "events=[" + strings.Join(parts, " ") + "]"
}

// emitOOMKill reports one detection: metric increments plus a log line.
// Metric attributes stay low-cardinality (level only); process attribution
// lives in the log because pids churn and comms are user-controlled.
func emitOOMKill(ctx context.Context, src oomEventSource, context string, delta oomDelta, exited []oomExited, bootTime, now int64) {
	if oomKills != nil && delta.total > 0 {
		if delta.haveLocal {
			if delta.local > 0 {
				oomKills.Add(ctx, delta.local, metric.WithAttributes(oomLevelAttrs("local")...))
			}
			if delta.descendant > 0 {
				oomKills.Add(ctx, delta.descendant, metric.WithAttributes(oomLevelAttrs("descendant")...))
			}
		} else {
			oomKills.Add(ctx, delta.total, metric.WithAttributes(sharedAttrs()...))
		}
	}

	summary := formatExited(exited, bootTime, now)
	if delta.haveLocal {
		log.Warn("oom: detected %d OOM kill(s) (local=%d, descendant=%d, source=%s, cgroup=%s%s)%s",
			delta.total, delta.local, delta.descendant, src.Mode, src.CgroupPath, context, summary)
	} else {
		log.Warn("oom: detected %d OOM kill(s) (source=%s, cgroup=%s%s)%s",
			delta.total, src.Mode, src.CgroupPath, context, summary)
	}
}

func oomLevelAttrs(level string) []attribute.KeyValue {
	attrs := append([]attribute.KeyValue{}, sharedAttrs()...)
	return append(attrs, attribute.String("level", level))
}

// formatExited renders exited processes for the log line, capped at
// oomMaxExited.
func formatExited(exited []oomExited, bootTime, now int64) string {
	if len(exited) == 0 {
		return ""
	}
	shown := exited
	extra := 0
	if len(exited) > oomMaxExited {
		shown = exited[:oomMaxExited]
		extra = len(exited) - oomMaxExited
	}
	parts := make([]string, 0, len(shown)+1)
	for _, process := range shown {
		entry := fmt.Sprintf("pid=%d comm=%s ppid=%d rss=%s",
			process.Pid, process.Proc.Comm, process.Proc.Ppid, humanBytes(process.Proc.RSSBytes))
		if age := victimAge(bootTime, process.Proc.StartTicks, now); age >= 0 {
			entry += fmt.Sprintf(" age=%ds", age)
		}
		parts = append(parts, entry)
	}
	if extra > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", extra))
	}
	return " exited=[" + strings.Join(parts, ", ") + "]"
}

// humanBytes renders a byte count with MiB/GiB units for logs.
func humanBytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
}
