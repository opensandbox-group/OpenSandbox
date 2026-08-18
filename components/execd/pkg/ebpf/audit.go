//go:build ebpf

// Copyright 2026 Alibaba Group Holding Ltd.
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

// OSEP-0018 §5: opt-in eBPF observation of exec / connect / privilege
// events, scoped to the sandbox cgroup, written as JSONL to a rotating
// audit file. Compiled only into the execd-ebpf build variant (CGO +
// cilium/ebpf); the default static image never contains this code.

package ebpf

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	defaultAuditFile = "/var/log/opensandbox/ebpf-audit.jsonl"

	// Capability numbers (linux/capability.h).
	capBpf     = 39
	capPerfmon = 38
)

// Event is the JSONL record: a stable common envelope plus per-kind fields
// (OSEP-0018 §5).
type Event struct {
	TS        string `json:"ts"`
	Event     string `json:"event"` // exec | connect | privilege
	SandboxID string `json:"sandbox_id"`
	PID       uint32 `json:"pid"`
	Comm      string `json:"comm"`

	// exec
	Filename string `json:"filename,omitempty"`
	PPID     uint32 `json:"ppid,omitempty"`

	// connect
	DstIP   string `json:"dst_ip,omitempty"`
	DstPort uint16 `json:"dst_port,omitempty"`
	Proto   string `json:"proto,omitempty"`

	// privilege
	OldUID   uint32   `json:"old_uid,omitempty"`
	NewUID   uint32   `json:"new_uid,omitempty"`
	OldGID   uint32   `json:"old_gid,omitempty"`
	NewGID   uint32   `json:"new_gid,omitempty"`
	CapAdded []string `json:"cap_added,omitempty"`
}

// Packed BPF event sizes (must match audit.bpf.c).
const (
	sizeEventExec      = 4 + 4 + 16 + 64
	sizeEventConnect   = 4 + 16 + 16 + 2
	sizeEventPrivilege = 4 + 16 + 4*4 + 8
)

// Observer consumes ringbuf events and appends them to the audit file.
type Observer struct {
	mu        sync.Mutex
	logger    *lumberjack.Logger
	sandboxID string
	kinds     map[string]bool
	reader    *ringbuf.Reader
	objs      *auditObjects
	links     []link.Link
	closed    chan struct{}
	closeOnce sync.Once
}

// Init activates the observer from the isolation config. The returned state
// and message describe what is actually enforced (for the capabilities
// endpoint).
func Init(cfg *isolation.EbpfConfig, sandboxID string) (state, message string) {
	disabled := func(msg string) (string, string) {
		return "disabled", msg
	}
	if cfg == nil || !cfg.Enabled {
		return disabled("eBPF observation is not enabled ([ebpf] enabled = false)")
	}
	if sandboxID == "" {
		return "unsupported",
			"eBPF observation cannot attribute audit records: OPENSANDBOX_ID is not set " +
				"(pool fast-path allocations without a task template cannot inject it); " +
				"set it via the runtime env to enable sandbox_id attribution"
	}
	if !effectiveCapsHave(capBpf) || !effectiveCapsHave(capPerfmon) {
		return "unsupported",
			"eBPF observation requires CAP_BPF + CAP_PERFMON (execd-ebpf build with a privileged container)"
	}
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return "unsupported",
			"eBPF observation requires a BTF-capable kernel (no /sys/kernel/btf/vmlinux); under gVisor/Kata the host kernel is not attachable"
	}
	cgroupID, err := currentCgroupID()
	if err != nil {
		return "degraded", fmt.Sprintf("eBPF observation cannot scope to the sandbox cgroup: %v", err)
	}

	observer, err := newObserver(cfg, sandboxID, cgroupID)
	if err != nil {
		return "degraded", fmt.Sprintf("eBPF observation failed to start: %v", err)
	}
	observer.start()
	return "active", fmt.Sprintf("eBPF observation active (cgroup %d, audit file %s)", cgroupID, observer.logger.Filename)
}

func newObserver(cfg *isolation.EbpfConfig, sandboxID string, cgroupID uint64) (*Observer, error) {
	_ = rlimit.RemoveMemlock()

	spec, err := loadAudit()
	if err != nil {
		return nil, fmt.Errorf("load audit programs: %w", err)
	}
	// Pin the cgroup filter; events outside the sandbox are dropped.
	if err := spec.RewriteConstants(map[string]interface{}{
		"target_cgroup": cgroupID,
	}); err != nil {
		return nil, fmt.Errorf("set audit cgroup filter: %w", err)
	}

	var objs auditObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return nil, fmt.Errorf("assign audit programs: %w", err)
	}

	auditFile := cfg.AuditFile
	if auditFile == "" {
		auditFile = defaultAuditFile
	}
	if err := os.MkdirAll(dirOf(auditFile), 0o755); err != nil {
		objs.Close()
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	logger := &lumberjack.Logger{
		Filename:   auditFile,
		MaxSize:    100, // MB
		MaxBackups: 3,
		MaxAge:     7, // days
	}

	var links []link.Link
	attached := map[string]bool{}
	attach := func(kind string, prog *ebpf.Program, attachFn func() (link.Link, error)) {
		if prog == nil {
			return
		}
		l, err := attachFn()
		if err != nil {
			log.Warn("ebpf: attach %s: %v", kind, err)
			return
		}
		links = append(links, l)
		attached[kind] = true
	}
	attach("exec", objs.OnExec, func() (link.Link, error) {
		return link.Tracepoint("sched", "sched_process_exec", objs.OnExec, nil)
	})
	attach("connect", objs.OnConnect, func() (link.Link, error) {
		return link.Tracepoint("sock", "inet_sock_set_state", objs.OnConnect, nil)
	})
	attach("privilege", objs.OnCommitCreds, func() (link.Link, error) {
		return link.Kprobe("commit_creds", objs.OnCommitCreds, nil)
	})

	reader, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		for _, l := range links {
			_ = l.Close()
		}
		objs.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	kinds := map[string]bool{}
	for _, kind := range cfg.Observe {
		kinds[kind] = true
	}
	if len(kinds) == 0 {
		for _, kind := range []string{"exec", "connect", "privilege"} {
			kinds[kind] = true
		}
	}
	for kind := range kinds {
		if !attached[kind] {
			// Release every already-attached hook and the ringbuf reader so
			// partially attached programs do not stay live after the
			// degraded report.
			for _, l := range links {
				_ = l.Close()
			}
			reader.Close()
			objs.Close()
			return nil, fmt.Errorf("requested observer hook %q could not be attached", kind)
		}
	}

	return &Observer{
		logger:    logger,
		sandboxID: sandboxID,
		kinds:     kinds,
		reader:    reader,
		objs:      &objs,
		links:     links,
		closed:    make(chan struct{}),
	}, nil
}

func (o *Observer) start() {
	go func() {
		defer o.Close()
		for {
			record, err := o.reader.Read()
			if err != nil {
				if err == ringbuf.ErrClosed {
					return
				}
				log.Warn("ebpf: ringbuf read: %v", err)
				continue
			}
			o.handleRecord(record.RawSample)
		}
	}()
}

// Close stops the observer and releases all BPF resources.
func (o *Observer) Close() {
	o.closeOnce.Do(func() {
		close(o.closed)
		_ = o.reader.Close()
		for _, l := range o.links {
			_ = l.Close()
		}
		if o.objs != nil {
			o.objs.Close()
		}
		_ = o.logger.Close()
	})
}

func (o *Observer) handleRecord(raw []byte) {
	event, ok := decodeEvent(raw)
	if !ok {
		log.Warn("ebpf: unknown event size %d", len(raw))
		return
	}
	if !o.kinds[event.Event] {
		return
	}
	event.SandboxID = o.sandboxID
	line, err := json.Marshal(event)
	if err != nil {
		log.Warn("ebpf: marshal event: %v", err)
		return
	}
	o.mu.Lock()
	if _, err := o.logger.Write(append(line, '\n')); err != nil {
		log.Error("ebpf: audit write failed: %v", err)
	}
	o.mu.Unlock()
}

func decodeEvent(raw []byte) (Event, bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	switch len(raw) {
	case sizeEventExec:
		ev := Event{TS: now, Event: "exec", PID: binary.LittleEndian.Uint32(raw[0:4])}
		ev.PPID = binary.LittleEndian.Uint32(raw[4:8])
		ev.Comm = cstring(raw[8:24])
		ev.Filename = cstring(raw[24:88])
		return ev, true
	case sizeEventConnect:
		ev := Event{TS: now, Event: "connect", PID: binary.LittleEndian.Uint32(raw[0:4])}
		ev.Comm = cstring(raw[4:20])
		ip := raw[20:36]
		ev.DstIP = formatIP(ip)
		ev.DstPort = binary.BigEndian.Uint16(raw[36:38])
		ev.Proto = "tcp"
		return ev, true
	case sizeEventPrivilege:
		ev := Event{TS: now, Event: "privilege", PID: binary.LittleEndian.Uint32(raw[0:4])}
		ev.Comm = cstring(raw[4:20])
		ev.OldUID = binary.LittleEndian.Uint32(raw[20:24])
		ev.NewUID = binary.LittleEndian.Uint32(raw[24:28])
		ev.OldGID = binary.LittleEndian.Uint32(raw[28:32])
		ev.NewGID = binary.LittleEndian.Uint32(raw[32:36])
		ev.CapAdded = capsFromBits(binary.LittleEndian.Uint64(raw[36:44]))
		return ev, true
	default:
		return Event{}, false
	}
}

func cstring(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func formatIP(raw []byte) string {
	// IPv4 is stored in the last 4 bytes of the 16-byte field.
	if raw[0] == 0 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 &&
		raw[4] == 0 && raw[5] == 0 && raw[6] == 0 && raw[7] == 0 &&
		raw[8] == 0 && raw[9] == 0 && raw[10] == 0xff && raw[11] == 0xff {
		return net.IPv4(raw[12], raw[13], raw[14], raw[15]).String()
	}
	return net.IP(raw).String()
}

var capNames = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER",
	"CAP_FSETID", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP",
	"CAP_LINUX_IMMUTABLE", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST",
	"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_IPC_LOCK", "CAP_IPC_OWNER",
	"CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT", "CAP_SYS_PTRACE",
	"CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE",
	"CAP_SYS_RESOURCE", "CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD",
	"CAP_LEASE", "CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL", "CAP_SETFCAP",
	"CAP_MAC_OVERRIDE", "CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM",
	"CAP_BLOCK_SUSPEND", "CAP_AUDIT_READ", "CAP_PERFMON", "CAP_BPF",
	"CAP_CHECKPOINT_RESTORE",
}

func capsFromBits(bits uint64) []string {
	var caps []string
	for i, name := range capNames {
		if bits&(1<<i) != 0 {
			caps = append(caps, name)
		}
	}
	return caps
}

func effectiveCapsHave(cap uint32) bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			return false
		}
		return value&(1<<cap) != 0
	}
	return false
}

// currentCgroupID returns the sandbox's cgroup v2 id (the inode number of
// the cgroup directory), used to scope the observation. The /proc/self/
// cgroup path is relative to the cgroup hierarchy root, so it must be
// resolved under the cgroup v2 mount (e.g. /sys/fs/cgroup) — the cgroup id
// equals the inode number of that directory in cgroupfs.
func currentCgroupID() (uint64, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, err
	}
	path := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			path = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if path == "" {
		return 0, fmt.Errorf("no cgroup v2 hierarchy in /proc/self/cgroup")
	}
	mount, err := cgroupV2Mount()
	if err != nil {
		return 0, err
	}
	full := filepath.Join(mount, strings.TrimPrefix(path, "/"))
	info, err := os.Stat(full)
	if err != nil {
		return 0, fmt.Errorf("stat cgroup %s: %w", full, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat cgroup %s: unexpected type", full)
	}
	return stat.Ino, nil
}

// cgroupV2Mount locates the cgroup v2 filesystem mount.
func cgroupV2Mount() (string, error) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "/sys/fs/cgroup", nil
	}
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 mount found")
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}
