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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type bwrapLifecycleArgv struct {
	gateExecFD string
	controlFD  string
	blockFD    string
	statusFD   string
}

// buildArgv constructs the legacy bwrap command line from wrap options.
func buildArgv(opts WrapOptions, seccompFd string) ([]string, error) {
	return buildArgvWithLifecycle(opts, seccompFd, nil)
}

// buildArgvWithLifecycle constructs the bwrap command line and, when lifecycle
// is non-nil, executes the native fail-closed workload gate directly through
// its inherited descriptor.
func buildArgvWithLifecycle(
	opts WrapOptions,
	seccompFd string,
	lifecycle *bwrapLifecycleArgv,
) ([]string, error) {
	if err := validateWrapOptions(opts); err != nil {
		return nil, err
	}

	useUserns := opts.UidMode == UidModeUserns

	var argv []string

	// 1. Namespace flags.
	argv = append(argv, bwrapNamespaceSegment(opts, useUserns)...)

	// 2. Root filesystem (read-only).
	argv = append(argv, "--ro-bind", "/", "/")

	// 3. /tmp — skip if workspace is /tmp (workspace bind would override).
	if filepath.Clean(opts.Workspace.Path) != "/tmp" {
		argv = append(argv, bwrapTmpSegment(opts.Profile)...)
	}

	// 4–6. Virtual filesystems. Lifecycle mode installs procfs after all
	// caller-controlled mounts so /proc/self/fd remains the trusted execution
	// path for the native gate descriptor.
	argv = append(argv, "--tmpfs", "/run", "--dev", "/dev")
	if lifecycle == nil {
		argv = append(argv, "--proc", "/proc")
	}

	// 7. Workspace.
	wsArgv, err := bwrapWorkspaceSegment(opts)
	if err != nil {
		return nil, err
	}
	argv = append(argv, wsArgv...)

	// Hide upper root to prevent cross-session access.
	if opts.UpperDir != "" {
		upperRoot := filepath.Dir(filepath.Dir(opts.UpperDir))
		argv = append(argv, "--tmpfs", upperRoot)
	}

	// 8. Extra writable paths.
	for _, p := range opts.ExtraWritable {
		argv = append(argv, "--bind", p, p)
	}

	// 8b. Explicit source→dest bind mounts.
	for _, b := range opts.Binds {
		dest := b.Dest
		if dest == "" {
			dest = b.Source
		}
		flag := "--bind"
		if b.ReadOnly {
			flag = "--ro-bind"
		}
		argv = append(argv, flag, b.Source, dest)
	}

	// Restore trusted procfs after every caller-controlled mount, then execute
	// the verified gate through its inherited descriptor. Static mount aliases
	// therefore cannot replace the gate between validation and execution.
	//
	// The isolated-session MVP treats processes sharing the parent sandbox
	// mount namespace as one trusted owner. Defending against that owner
	// concurrently replacing the proc mount ancestor still requires a future
	// execveat-based launcher.
	if lifecycle != nil {
		argv = append(argv, "--proc", "/proc")
	}

	// 9. Environment.
	argv = append(argv, bwrapEnvSegment(opts.EnvPassthrough)...)

	// 10. Seccomp.
	if seccompFd != "" {
		argv = append(argv, "--seccomp", seccompFd)
	}

	// 11. Lifecycle: kill sandbox when execd dies.
	// Note: --new-session is intentionally omitted. bwrap is launched with
	// SysProcAttr{Setpgid: true}, making it a process-group leader, and
	// setsid(2) returns EPERM for a group leader — it would fail every
	// session start. Process-group isolation from Setpgid is sufficient.
	argv = append(argv, "--die-with-parent")
	if lifecycle != nil {
		argv = append(
			argv,
			"--block-fd", lifecycle.blockFD,
			"--json-status-fd", lifecycle.statusFD,
		)
	}

	// 12. Separator + fail-closed gate + identity switch.
	argv = append(argv, "--")

	// In setpriv mode the trusted gate must run before credentials are dropped.
	// Execd authenticates and inspects the blocked gate through /proc; moving
	// setpriv after the gate keeps those checks available without granting
	// CAP_SYS_PTRACE. Once READY arrives, the gate execs setpriv and the caller's
	// command in the same PID and namespaces.
	if lifecycle != nil {
		argv = append(
			argv,
			"/proc/self/fd/"+lifecycle.gateExecFD,
			lifecycle.controlFD,
			lifecycle.gateExecFD,
			"--",
		)
	}

	// In userns mode, uid/gid are set via --uid/--gid in segment 1.
	if !useUserns {
		uid := uint32(os.Getuid())
		gid := uint32(os.Getgid())
		if opts.Uid != nil {
			uid = *opts.Uid
		}
		if opts.Gid != nil {
			gid = *opts.Gid
		}

		if uid != 0 || gid != 0 {
			setprivArgv := []string{
				"setpriv",
				fmt.Sprintf("--reuid=%d", uid),
				fmt.Sprintf("--regid=%d", gid),
				"--clear-groups",
			}
			argv = append(argv, setprivArgv...)
		}
	}

	return argv, nil
}

func bwrapNamespaceSegment(opts WrapOptions, useUserns bool) []string {
	var argv []string
	if useUserns {
		argv = append(argv, "--unshare-user")
		// --disable-userns is unsupported by the setuid build of bwrap;
		// only add it for the non-setuid binary.
		if !bwrapIsSetuid {
			argv = append(argv, "--disable-userns")
		}
	}
	argv = append(argv, "--unshare-pid", "--unshare-uts", "--hostname", "sandbox", "--unshare-ipc", "--unshare-cgroup")
	if !opts.ShareNet {
		argv = append(argv, "--unshare-net")
	}
	if useUserns {
		uid := uint32(os.Getuid())
		gid := uint32(os.Getgid())
		if opts.Uid != nil {
			uid = *opts.Uid
		}
		if opts.Gid != nil {
			gid = *opts.Gid
		}
		argv = append(argv,
			"--uid", strconv.FormatUint(uint64(uid), 10),
			"--gid", strconv.FormatUint(uint64(gid), 10),
		)
	}
	return argv
}

// validateWrapOptions checks for invalid or conflicting options.
func validateWrapOptions(opts WrapOptions) error {
	if opts.Workspace.Path == "" {
		return errors.New("isolation: workspace.path is required")
	}
	if !opts.Profile.Valid() {
		return fmt.Errorf("isolation: unknown profile %q", opts.Profile)
	}
	if !opts.Workspace.Mode.Valid() {
		return fmt.Errorf("isolation: unknown workspace mode %q", opts.Workspace.Mode)
	}
	if !opts.EnvPassthrough.Mode.Valid() && opts.EnvPassthrough.Mode != "" {
		return fmt.Errorf("isolation: unknown env mode %q", opts.EnvPassthrough.Mode)
	}
	if opts.UidMode != "" && !opts.UidMode.Valid() {
		return fmt.Errorf("isolation: unknown uid mode %q", opts.UidMode)
	}
	for _, b := range opts.Binds {
		if b.Source == "" {
			return errors.New("isolation: bind.source is required")
		}
		if !filepath.IsAbs(b.Source) {
			return fmt.Errorf("isolation: bind.source %q must be an absolute path", b.Source)
		}
		if b.Dest != "" && !filepath.IsAbs(b.Dest) {
			return fmt.Errorf("isolation: bind.dest %q must be an absolute path", b.Dest)
		}
	}
	return nil
}

// bwrapTmpSegment returns the /tmp mount args for the given profile.
func bwrapTmpSegment(p Profile) []string {
	switch p {
	case ProfileStrict:
		return []string{"--tmpfs", "/tmp"}
	default:
		// balanced and others: share container /tmp.
		return []string{"--bind", "/tmp", "/tmp"}
	}
}

// bwrapWorkspaceSegment returns mount args for the workspace.
func bwrapWorkspaceSegment(opts WrapOptions) ([]string, error) {
	ws := opts.Workspace

	switch ws.Mode {
	case WorkspaceRW:
		return []string{"--bind", ws.Path, ws.Path}, nil

	case WorkspaceRO:
		return []string{"--ro-bind", ws.Path, ws.Path}, nil

	case WorkspaceOverlay:
		if opts.UpperDir == "" {
			// tmpfs upper — ephemeral. --tmp-overlay DEST (bwrap v0.11.x).
			return []string{"--overlay-src", ws.Path, "--tmp-overlay", ws.Path}, nil
		}
		workDir := opts.WorkDir
		if workDir == "" {
			workDir = opts.UpperDir + "-work"
		}
		// --overlay-src LOWER --overlay RWSRC WORKDIR DEST
		return []string{"--overlay-src", ws.Path, "--overlay", opts.UpperDir, workDir, ws.Path}, nil

	default:
		return nil, fmt.Errorf("isolation: unknown workspace mode %q", ws.Mode)
	}
}

func unsetExecdConfigEnv() []string {
	argv := make([]string, 0, 2*len(execdConfigEnvBlacklist))
	for _, key := range execdConfigEnvBlacklist {
		argv = append(argv, "--unsetenv", key)
	}
	return argv
}

// unsetBlacklistedEnv returns --unsetenv args for all env vars matching strictEnvBlacklist.
func unsetBlacklistedEnv() []string {
	var argv []string
	for _, pattern := range strictEnvBlacklist {
		for _, env := range os.Environ() {
			kv := strings.SplitN(env, "=", 2)
			if matchEnvPattern(kv[0], pattern) {
				argv = append(argv, "--unsetenv", kv[0])
			}
		}
	}
	return argv
}

// bwrapEnvSegment returns environment passthrough args. execd's own config
// env (execdConfigEnvBlacklist) is always stripped, regardless of mode.
func bwrapEnvSegment(spec EnvSpec) []string {
	if spec.Mode == "" {
		argv := unsetExecdConfigEnv()
		return append(argv, unsetBlacklistedEnv()...)
	}

	switch spec.Mode {
	case EnvModeDeny:
		argv := unsetExecdConfigEnv()
		for _, key := range spec.Keys {
			argv = append(argv, "--unsetenv", key)
		}
		if len(spec.Keys) == 0 {
			argv = append(argv, unsetBlacklistedEnv()...)
		}
		return argv

	case EnvModeAllow:
		// --clearenv wipes everything; refuse to re-inject execd config env
		// even if the caller allow-lists it.
		argv := []string{"--clearenv"}
		blacklist := make(map[string]struct{}, len(execdConfigEnvBlacklist))
		for _, k := range execdConfigEnvBlacklist {
			blacklist[k] = struct{}{}
		}
		for _, key := range spec.Keys {
			if _, blocked := blacklist[key]; blocked {
				continue
			}
			if val, ok := os.LookupEnv(key); ok {
				argv = append(argv, "--setenv", key, val)
			}
		}
		return argv

	default:
		return nil
	}
}

// strictEnvBlacklist defines glob patterns stripped in strict profile.
var strictEnvBlacklist = []string{
	"*_API_KEY", "*_TOKEN", "*_SECRET", "*_PASSWORD",
	"AWS_*", "ALI_*", "ALIYUN_*", "K8S_*", "KUBE_*",
}

// matchEnvPattern performs a simple case-insensitive glob match.
func matchEnvPattern(name, pattern string) bool {
	name = strings.ToUpper(name)
	pattern = strings.ToUpper(pattern)

	// Wildcard-only: *TOKEN* → contains TOKEN
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		mid := pattern[1 : len(pattern)-1]
		return strings.Contains(name, mid)
	}
	// Suffix wildcard: *_TOKEN → has suffix _TOKEN
	if strings.HasPrefix(pattern, "*") {
		suffix := pattern[1:]
		return strings.HasSuffix(name, suffix)
	}
	// Prefix wildcard: AWS_* → has prefix AWS_
	if strings.HasSuffix(pattern, "*") {
		prefix := pattern[:len(pattern)-1]
		return strings.HasPrefix(name, prefix)
	}
	// Exact match.
	return name == pattern
}

// Wrap rewrites cmd to execute under bwrap.
func wrapWithArgv(cmd *exec.Cmd, bwrapPath string, argv []string) {
	// Prepend bwrap argv before the original command.
	// argv already contains the bwrap separator and any lifecycle gate or
	// identity-switch prefix. The original cmd.Args[0] follows that prefix.
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(argv)+len(userArgs))
	cmd.Args = append(cmd.Args, bwrapPath)
	cmd.Args = append(cmd.Args, argv...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = bwrapPath
}
