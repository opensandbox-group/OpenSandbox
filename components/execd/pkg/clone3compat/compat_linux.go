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

package clone3compat

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"golang.org/x/sys/unix"
)

const (
	envCompat  = "EXECD_CLONE3_COMPAT"
	envApplied = "_EXECD_CLONE3_COMPAT_APPLIED"
)

// MaybeApply optionally installs a seccomp rule so clone3 returns ENOSYS, matching the
// behavior of https://github.com/AkihiroSuda/clone3-workaround .
// It returns true if this process is running with that compatibility active (including
// a post-reexec process that inherited the seccomp filter).
func MaybeApply() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(envCompat)))
	switch mode {
	case "", "0", "false", "off", "no":
		return false
	case "1", "true", "yes", "on":
		if err := loadClone3EnosysFilter(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "execd: %v\n", err)
			os.Exit(1)
		}
		return true
	case "reexec":
		if os.Getenv(envApplied) == "1" {
			return true
		}
		if err := loadClone3EnosysFilter(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "execd: %v\n", err)
			os.Exit(1)
		}
		if err := os.Setenv(envApplied, "1"); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "execd: clone3 compat: set %s: %v\n", envApplied, err)
			os.Exit(1)
		}
		exe, err := os.Readlink("/proc/self/exe")
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "execd: clone3 compat: readlink /proc/self/exe: %v\n", err)
			os.Exit(1)
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		if err := unix.Exec(exe, os.Args, os.Environ()); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "execd: clone3 compat: exec: %v\n", err)
			os.Exit(1)
		}
		panic("unreachable") // Exec replaces this process.
	default:
		_, _ = fmt.Fprintf(os.Stderr, "execd: invalid %s=%q (use 1, true, or reexec)\n", envCompat, os.Getenv(envCompat))
		os.Exit(1)
	}

	return false
}

func loadClone3EnosysFilter() error {
	if !seccomp.Supported() {
		return errors.New("clone3 compat: seccomp is not available on this kernel")
	}
	f := seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy: seccomp.Policy{
			DefaultAction: seccomp.ActionAllow,
			Syscalls: []seccomp.SyscallGroup{
				{
					Names: []string{"clone3"},
					// Not plain ActionErrno: assembler defaults errno to EPERM.
					Action: seccomp.ActionErrno | seccomp.Action(syscall.ENOSYS),
				},
			},
		},
	}
	if err := seccomp.LoadFilter(f); err != nil {
		return fmt.Errorf("clone3 compat: %w", err)
	}
	return nil
}
