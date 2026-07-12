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

//go:build linux

package isolation

import (
	"fmt"
	"syscall"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// denylistSyscalls lists syscall names to block. Syscalls not present on the
// current architecture are silently skipped.
var denylistSyscalls = []string{
	// Filesystem manipulation
	"mount", "umount2", "chroot", "pivot_root",

	// Process introspection / manipulation
	"ptrace", "process_vm_readv", "process_vm_writev",
	"kcmp",

	// Kernel module loading
	"init_module", "finit_module", "delete_module",

	// BPF / seccomp manipulation
	"bpf", "seccomp",

	// Execution domain
	"personality",

	// Privilege UID/GID changes omitted: setpriv needs setresuid/setresgid
	// to drop from root to the requested UID/GID. After the drop, processes
	// have no CAP_SETUID and cannot regain privileges.

	// Kernel key management
	"add_key", "request_key", "keyctl",

	// I/O privilege
	"iopl", "ioperm",

	// System state
	"reboot", "syslog",
	"swapon", "swapoff",

	// Namespace manipulation (already in bwrap namespace)
	"setns", "unshare",

	// Handle-based operations
	"name_to_handle_at", "open_by_handle_at",

	// Other potentially dangerous
	"userfaultfd",
	"kexec_load", "kexec_file_load",
	"acct",
}

// generateSeccompDenyBPF returns BPF bytecode for a default-allow,
// deny-listed syscall filter. The returned bytes are in struct sock_filter
// format (8 bytes per instruction, native endian).
//
// When override is nil the built-in denylistSyscalls is used. When non-nil,
// override.Deny completely replaces the built-in list.
func generateSeccompDenyBPF(override *SeccompOverride) ([]byte, error) {
	archInfo, err := arch.GetInfo("")
	if err != nil {
		return nil, fmt.Errorf("seccomp: detect arch: %w", err)
	}

	denylist := denylistSyscalls
	if override != nil {
		denylist = override.Deny
	}

	// Filter denylist to syscalls that exist on this architecture.
	names := filterKnownSyscalls(archInfo, denylist)
	if len(names) == 0 {
		return nil, nil
	}

	policy := seccomp.Policy{
		DefaultAction: seccomp.ActionAllow,
		Syscalls: []seccomp.SyscallGroup{
			{
				Names:  names,
				Action: seccomp.ActionErrno | seccomp.Action(syscall.EACCES),
			},
		},
	}

	instructions, err := policy.Assemble()
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble policy: %w", err)
	}

	raw, err := bpf.Assemble(instructions)
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble BPF: %w", err)
	}

	// Serialize to struct sock_filter bytes (8 bytes per instruction).
	// Little-endian since amd64/arm64 targets are LE.
	buf := make([]byte, len(raw)*8)
	for i, ri := range raw {
		off := i * 8
		buf[off] = byte(ri.Op)
		buf[off+1] = byte(ri.Op >> 8)
		buf[off+2] = ri.Jt
		buf[off+3] = ri.Jf
		buf[off+4] = byte(ri.K)
		buf[off+5] = byte(ri.K >> 8)
		buf[off+6] = byte(ri.K >> 16)
		buf[off+7] = byte(ri.K >> 24)
	}

	return buf, nil
}

// filterKnownSyscalls returns names that exist in the architecture's table.
func filterKnownSyscalls(archInfo *arch.Info, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := archInfo.SyscallNames[name]; ok {
			out = append(out, name)
		}
	}
	return out
}
