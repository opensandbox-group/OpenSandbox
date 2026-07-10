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

// buildArgv constructs the bwrap command line from wrap options.
func buildArgv(opts WrapOptions, seccompFd string) ([]string, error) {
	if err := validateWrapOptions(opts); err != nil {
		return nil, err
	}

	useUserns := opts.UidMode == UidModeUserns

	var argv []string

	// 1. Namespace flags.
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

	// 2. Root filesystem (read-only).
	argv = append(argv, "--ro-bind", "/", "/")

	// 3. /tmp — skip if workspace is /tmp (workspace bind would override).
	if filepath.Clean(opts.Workspace.Path) != "/tmp" {
		argv = append(argv, bwrapTmpSegment(opts.Profile)...)
	}

	// 4–6. Virtual filesystems.
	argv = append(argv, "--tmpfs", "/run", "--dev", "/dev", "--proc", "/proc")

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

	// 12. Separator + identity switch.
	argv = append(argv, "--")

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

// bwrapEnvSegment returns environment passthrough args.
func bwrapEnvSegment(spec EnvSpec) []string {
	if spec.Mode == "" {
		return unsetBlacklistedEnv()
	}

	switch spec.Mode {
	case EnvModeDeny:
		var argv []string
		for _, key := range spec.Keys {
			argv = append(argv, "--unsetenv", key)
		}
		if len(spec.Keys) == 0 {
			argv = append(argv, unsetBlacklistedEnv()...)
		}
		return argv

	case EnvModeAllow:
		// Clear environment, inject only allow-listed keys.
		argv := []string{"--clearenv"}
		for _, key := range spec.Keys {
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
	// argv already ends with ["--", "setpriv", ...] and the original
	// cmd.Args[0] is the user command after setpriv.
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(argv)+len(userArgs))
	cmd.Args = append(cmd.Args, bwrapPath)
	cmd.Args = append(cmd.Args, argv...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = bwrapPath
}
