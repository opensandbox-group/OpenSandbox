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
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func uidPtr(n uint32) *uint32 { return &n }

// Argv builder tests (platform-independent)

func TestBuildArgv_NamespaceFlags(t *testing.T) {
	tests := []struct {
		name     string
		shareNet bool
		want     string // substring that must appear
		dontWant string // substring that must NOT appear
	}{
		{"share_net=true (default)", true, "", "--unshare-net"},
		{"share_net=false", false, "--unshare-net", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := basicWrapOpts()
			opts.ShareNet = tt.shareNet
			argv, err := buildArgv(opts, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			s := strings.Join(argv, " ")
			if tt.want != "" && !strings.Contains(s, tt.want) {
				t.Errorf("argv missing %q:\n  %s", tt.want, s)
			}
			if tt.dontWant != "" && strings.Contains(s, tt.dontWant) {
				t.Errorf("argv contains %q but should not:\n  %s", tt.dontWant, s)
			}
		})
	}
}

func TestBuildArgv_TmpSegment(t *testing.T) {
	tests := []struct {
		profile Profile
		want    string
		dont    string
	}{
		{ProfileStrict, "--tmpfs /tmp", "--bind /tmp /tmp"},
		{ProfileBalanced, "--bind /tmp /tmp", "--tmpfs /tmp"},
	}

	for _, tt := range tests {
		t.Run(string(tt.profile), func(t *testing.T) {
			opts := basicWrapOpts()
			opts.Profile = tt.profile
			argv, err := buildArgv(opts, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			s := strings.Join(argv, " ")
			if !strings.Contains(s, tt.want) {
				t.Errorf("%s: missing %q:\n  %s", tt.profile, tt.want, s)
			}
			if tt.dont != "" && strings.Contains(s, tt.dont) {
				t.Errorf("%s: should not contain %q:\n  %s", tt.profile, tt.dont, s)
			}
		})
	}
}

func TestBuildArgv_WorkspaceSegment(t *testing.T) {
	ws := func(mode WorkspaceMode) WrapOptions {
		opts := basicWrapOpts()
		opts.Workspace.Mode = mode
		return opts
	}

	t.Run("rw", func(t *testing.T) {
		argv, err := buildArgv(ws(WorkspaceRW), "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--bind /workspace /workspace") {
			t.Error(s)
		}
	})

	t.Run("ro", func(t *testing.T) {
		argv, err := buildArgv(ws(WorkspaceRO), "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--ro-bind /workspace /workspace") {
			t.Error(s)
		}
	})

	t.Run("overlay_with_persist", func(t *testing.T) {
		opts := ws(WorkspaceOverlay)
		opts.UpperDir = "/var/lib/execd/isolation/abc"
		opts.WorkDir = "/var/lib/execd/isolation/abc-work"
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		for _, want := range []string{
			"--overlay",
			"/workspace",
			"/var/lib/execd/isolation/abc",
			"/var/lib/execd/isolation/abc-work",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("overlay_without_persist_tmpfs", func(t *testing.T) {
		opts := ws(WorkspaceOverlay)
		opts.UpperDir = "" // tmpfs upper
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--overlay-src") {
			t.Error("missing --overlay-src")
		}
	})
}

func TestBuildArgv_EnvPassthrough(t *testing.T) {
	t.Run("deny_with_keys", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.EnvPassthrough = EnvSpec{Mode: EnvModeDeny, Keys: []string{"SECRET", "TOKEN"}}
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--unsetenv SECRET") {
			t.Error(s)
		}
		if !strings.Contains(s, "--unsetenv TOKEN") {
			t.Error(s)
		}
	})

	t.Run("allow_with_clearenv", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.EnvPassthrough = EnvSpec{Mode: EnvModeAllow, Keys: []string{"PATH", "HOME"}}
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--clearenv") {
			t.Error("missing --clearenv")
		}
	})

	t.Run("empty_mode_no_env_args", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.EnvPassthrough = EnvSpec{} // empty mode
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if strings.Contains(s, "--clearenv") || strings.Contains(s, "--unsetenv") {
			t.Error("should not have env args for empty mode")
		}
	})
}

func TestBuildArgv_ExtraWritable(t *testing.T) {
	opts := basicWrapOpts()
	opts.ExtraWritable = []string{"/data", "/tmp/custom"}
	argv, err := buildArgv(opts, "")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(argv, " ")

	for _, p := range opts.ExtraWritable {
		// Each writable path generates "--bind $p $p"
		if strings.Count(s, p) < 2 {
			t.Errorf("missing bind for %q in:\n  %s", p, s)
		}
	}
}

func TestBuildArgv_Binds(t *testing.T) {
	t.Run("rw_src_to_dest", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Source: "/host/data", Dest: "/container/data"}}
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")
		assert.Contains(t, s, "--bind /host/data /container/data")
	})

	t.Run("readonly_uses_ro_bind", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Source: "/host/ro", Dest: "/mnt/ro", ReadOnly: true}}
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")
		assert.Contains(t, s, "--ro-bind /host/ro /mnt/ro")
	})

	t.Run("empty_dest_defaults_to_source", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Source: "/host/same"}}
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")
		assert.Contains(t, s, "--bind /host/same /host/same")
	})

	t.Run("validation_empty_source", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Dest: "/mnt/x"}}
		_, err := buildArgv(opts, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bind.source is required")
	})

	t.Run("validation_relative_source", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Source: "relative/path"}}
		_, err := buildArgv(opts, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an absolute path")
	})

	t.Run("validation_relative_dest", func(t *testing.T) {
		opts := basicWrapOpts()
		opts.Binds = []BindMount{{Source: "/host/a", Dest: "rel/dest"}}
		_, err := buildArgv(opts, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an absolute path")
	})
}

func TestBuildArgv_Setpriv(t *testing.T) {
	t.Run("default_uid_gid", func(t *testing.T) {
		opts := basicWrapOpts()
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "setpriv") {
			t.Error("missing setpriv")
		}
		if !strings.Contains(s, "--clear-groups") {
			t.Error("missing --clear-groups")
		}
	})

	t.Run("explicit_uid_gid", func(t *testing.T) {
		opts := basicWrapOpts()
		u, g := uint32(1001), uint32(1002)
		opts.Uid = &u
		opts.Gid = &g
		argv, err := buildArgv(opts, "")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(argv, " ")
		if !strings.Contains(s, "--reuid=1001") {
			t.Error("missing --reuid=1001")
		}
		if !strings.Contains(s, "--regid=1002") {
			t.Error("missing --regid=1002")
		}
	})
}

func TestBuildArgv_Userns(t *testing.T) {
	t.Run("userns_mode_uses_unshare_user", func(t *testing.T) {
		opts := basicWrapOpts()
		u, g := uint32(1000), uint32(1000)
		opts.Uid = &u
		opts.Gid = &g
		opts.UidMode = UidModeUserns
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")

		assert.Contains(t, s, "--unshare-user", "userns mode must include --unshare-user")
		assert.Contains(t, s, "--disable-userns", "userns mode must include --disable-userns")
		assert.Contains(t, s, "--uid 1000", "userns mode must include --uid")
		assert.Contains(t, s, "--gid 1000", "userns mode must include --gid")
	})

	t.Run("userns_mode_setuid_bwrap_skips_disable_userns", func(t *testing.T) {
		// --disable-userns is unsupported by the setuid build of bwrap.
		bwrapIsSetuid = true
		defer func() { bwrapIsSetuid = false }()

		opts := basicWrapOpts()
		opts.UidMode = UidModeUserns
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")

		assert.Contains(t, s, "--unshare-user", "userns mode must still include --unshare-user")
		assert.NotContains(t, s, "--disable-userns", "setuid bwrap must not include --disable-userns")
	})

	t.Run("userns_mode_no_setpriv", func(t *testing.T) {
		opts := basicWrapOpts()
		u, g := uint32(1000), uint32(1000)
		opts.Uid = &u
		opts.Gid = &g
		opts.UidMode = UidModeUserns
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")

		assert.NotContains(t, s, "setpriv", "userns mode must not include setpriv")
		assert.NotContains(t, s, "--reuid", "userns mode must not include --reuid")
		assert.NotContains(t, s, "--regid", "userns mode must not include --regid")
	})

	t.Run("setpriv_mode_no_unshare_user", func(t *testing.T) {
		opts := basicWrapOpts()
		u, g := uint32(1000), uint32(1000)
		opts.Uid = &u
		opts.Gid = &g
		opts.UidMode = UidModeSetpriv
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")

		assert.NotContains(t, s, "--unshare-user", "setpriv mode must not include --unshare-user")
		assert.NotContains(t, s, "--disable-userns", "setpriv mode must not include --disable-userns")
		assert.Contains(t, s, "setpriv", "setpriv mode must include setpriv")
		assert.Contains(t, s, "--reuid=1000", "setpriv mode must include --reuid")
		assert.Contains(t, s, "--regid=1000", "setpriv mode must include --regid")
	})

	t.Run("empty_uid_mode_defaults_to_setpriv", func(t *testing.T) {
		opts := basicWrapOpts()
		u, g := uint32(1000), uint32(1000)
		opts.Uid = &u
		opts.Gid = &g
		// UidMode is empty string — should behave like setpriv.
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)
		s := strings.Join(argv, " ")

		assert.NotContains(t, s, "--unshare-user")
		assert.Contains(t, s, "setpriv")
	})

	t.Run("userns_namespace_flag_order", func(t *testing.T) {
		opts := basicWrapOpts()
		u := uint32(1000)
		opts.Uid = &u
		opts.UidMode = UidModeUserns
		argv, err := buildArgv(opts, "")
		require.NoError(t, err)

		// --unshare-user must come before --unshare-pid (both in segment 1).
		idxUser := indexOf(argv, "--unshare-user")
		idxPid := indexOf(argv, "--unshare-pid")
		assert.Greater(t, idxPid, idxUser,
			"--unshare-user should appear before --unshare-pid")

		// --uid/--gid should come after --unshare-ipc (still in segment 1),
		// before segment 2 (--ro-bind).
		idxUid := indexOf(argv, "--uid")
		idxRoBind := indexOf(argv, "--ro-bind")
		assert.Greater(t, idxRoBind, idxUid,
			"--uid should appear before --ro-bind (segment 2)")
	})
}

func TestBuildArgv_Validation_UidMode(t *testing.T) {
	opts := basicWrapOpts()
	opts.UidMode = "bogus"
	_, err := buildArgv(opts, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown uid mode")
}

func TestBuildArgv_Seccomp(t *testing.T) {
	opts := basicWrapOpts()
	argv, err := buildArgv(opts, "3") // fd number passed to --seccomp
	require.NoError(t, err)
	s := strings.Join(argv, " ")
	assert.Contains(t, s, "--seccomp 3", "missing seccomp fd")
}

func TestBuildArgv_SegmentOrder(t *testing.T) {
	opts := basicWrapOpts()
	opts.Profile = ProfileStrict
	opts.Workspace.Mode = WorkspaceOverlay
	opts.UpperDir = "/tmp/upper"
	opts.ExtraWritable = []string{"/data"}
	opts.EnvPassthrough = EnvSpec{Mode: EnvModeDeny, Keys: []string{"TOKEN"}}

	argv, err := buildArgv(opts, "3") // fd number passed to --seccomp
	if err != nil {
		t.Fatal(err)
	}

	// Expected segment order. We track by scanning argv for each marker
	// and comparing the index of the first segment element.
	type seg struct {
		label string
		match string // single argv element
	}
	order := []seg{
		{"1.ns", "--unshare-pid"},
		{"2.rootfs", "--ro-bind"},
		{"3.tmp", "/tmp"},
		{"4.run", "/run"},
		{"5.dev", "--dev"},
		{"6.proc", "--proc"},
		{"7.workspace", "--overlay-src"},
		{"8.extra_writable", "--bind"},
		{"9.env", "--unsetenv"},
		{"10.seccomp", "--seccomp"},
		{"11.setpriv", "setpriv"},
	}

	lastIdx := -1
	for _, s := range order {
		idx := indexOf(argv, s.match)
		if idx < 0 {
			t.Errorf("segment %s (%q) not found in argv:\n  %v", s.label, s.match, argv)
			continue
		}
		if idx <= lastIdx {
			t.Errorf("segment %s (%q) at %d: should be after index %d", s.label, s.match, idx, lastIdx)
		}
		lastIdx = idx
	}
}

func TestBuildArgv_SegmentOrder_Userns(t *testing.T) {
	opts := basicWrapOpts()
	opts.Profile = ProfileStrict
	opts.Workspace.Mode = WorkspaceOverlay
	opts.UpperDir = "/tmp/upper"
	opts.ExtraWritable = []string{"/data"}
	opts.EnvPassthrough = EnvSpec{Mode: EnvModeDeny, Keys: []string{"TOKEN"}}
	u := uint32(1000)
	opts.Uid = &u
	opts.UidMode = UidModeUserns

	argv, err := buildArgv(opts, "3")
	require.NoError(t, err)

	type seg struct {
		label string
		match string
	}
	// In userns mode: --unshare-user comes first, no setpriv at the end.
	order := []seg{
		{"1.ns.userns", "--unshare-user"},
		{"1.ns.pid", "--unshare-pid"},
		{"2.rootfs", "--ro-bind"},
		{"3.tmp", "/tmp"},
		{"4.run", "/run"},
		{"5.dev", "--dev"},
		{"6.proc", "--proc"},
		{"7.workspace", "--overlay-src"},
		{"8.extra_writable", "--bind"},
		{"9.env", "--unsetenv"},
		{"10.seccomp", "--seccomp"},
	}

	lastIdx := -1
	for _, s := range order {
		idx := indexOf(argv, s.match)
		if idx < 0 {
			t.Errorf("segment %s (%q) not found in argv:\n  %v", s.label, s.match, argv)
			continue
		}
		if idx <= lastIdx {
			t.Errorf("segment %s (%q) at %d: should be after index %d", s.label, s.match, idx, lastIdx)
		}
		lastIdx = idx
	}

	// setpriv must NOT appear in userns mode.
	assert.Equal(t, -1, indexOf(argv, "setpriv"), "setpriv must not appear in userns mode")
}

func TestBuildArgv_Validation(t *testing.T) {
	tests := []struct {
		name string
		opts WrapOptions
		want string
	}{
		{"empty_workspace", WrapOptions{}, "workspace.path is required"},
		{"bad_profile", WrapOptions{Workspace: WorkspaceSpec{Path: "/ws", Mode: WorkspaceRW}, Profile: "bogus"}, "unknown profile"},
		{"bad_mode", WrapOptions{Profile: ProfileBalanced, Workspace: WorkspaceSpec{Path: "/ws", Mode: "bogus"}}, "unknown workspace mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildArgv(tt.opts, "")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

// Env pattern match tests

func TestMatchEnvPattern(t *testing.T) {
	tests := []struct {
		testName string
		envName  string
		pattern  string
		want     bool
	}{
		{"exact", "PATH", "PATH", true},
		{"suffix_wildcard_hit", "GITHUB_TOKEN", "*_TOKEN", true},
		{"suffix_wildcard_miss", "PATH", "*_TOKEN", false},
		{"prefix_wildcard_hit", "AWS_ACCESS_KEY_ID", "AWS_*", true},
		{"prefix_wildcard_miss", "PATH", "AWS_*", false},
		{"full_wildcard_hit", "MY_SECRET_KEY", "*SECRET*", true},
		{"full_wildcard_miss", "PATH", "*SECRET*", false},
		{"case_insensitive_exact", "PATH", "path", true},
		{"case_insensitive_pattern", "GITHUB_TOKEN", "*_token", true},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			got := matchEnvPattern(tt.envName, tt.pattern)
			if got != tt.want {
				t.Errorf("matchEnvPattern(%q, %q) = %v, want %v", tt.envName, tt.pattern, got, tt.want)
			}
		})
	}
}

// WrapWithArgv test

func TestWrapWithArgv(t *testing.T) {
	cmd := exec.Command("bash", "-c", "echo hello")

	argv := []string{
		"--unshare-pid", "--ro-bind", "/", "/",
		"--tmpfs", "/tmp", "--tmpfs", "/run",
		"--dev", "/dev", "--proc", "/proc",
		"--bind", "/workspace", "/workspace",
		"--", "setpriv", "--reuid=1000", "--regid=1000", "--clear-groups",
	}

	wrapWithArgv(cmd, "/usr/bin/bwrap", argv)

	if cmd.Path != "/usr/bin/bwrap" {
		t.Errorf("Path = %q, want /usr/bin/bwrap", cmd.Path)
	}

	if len(cmd.Args) < 3 {
		t.Fatalf("too few args: %v", cmd.Args)
	}

	if cmd.Args[0] != "/usr/bin/bwrap" {
		t.Errorf("Args[0] = %q, want /usr/bin/bwrap", cmd.Args[0])
	}

	// Original command args should be at the end.
	n := len(cmd.Args)
	if cmd.Args[n-1] != "echo hello" || cmd.Args[n-2] != "-c" || cmd.Args[n-3] != "bash" {
		t.Errorf("original args not preserved at end: %v", cmd.Args)
	}
}

// Profile / WorkspaceMode / EnvMode Valid tests

func TestProfile_Valid(t *testing.T) {
	if !ProfileStrict.Valid() {
		t.Error("strict should be valid")
	}
	if !ProfileBalanced.Valid() {
		t.Error("balanced should be valid")
	}
	if Profile("bogus").Valid() {
		t.Error("bogus should be invalid")
	}
}

func TestWorkspaceMode_Valid(t *testing.T) {
	for _, m := range []WorkspaceMode{WorkspaceRW, WorkspaceOverlay, WorkspaceRO} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	if WorkspaceMode("bogus").Valid() {
		t.Error("bogus should be invalid")
	}
}

func TestEnvMode_Valid(t *testing.T) {
	if !EnvModeDeny.Valid() {
		t.Error("deny should be valid")
	}
	if !EnvModeAllow.Valid() {
		t.Error("allow should be valid")
	}
	if EnvMode("bogus").Valid() {
		t.Error("bogus should be invalid")
	}
}

func TestUidMode_Valid(t *testing.T) {
	if !UidModeSetpriv.Valid() {
		t.Error("setpriv should be valid")
	}
	if !UidModeUserns.Valid() {
		t.Error("userns should be valid")
	}
	if UidMode("bogus").Valid() {
		t.Error("bogus should be invalid")
	}
}

// Helpers

func basicWrapOpts() WrapOptions {
	return WrapOptions{
		Profile:   ProfileBalanced,
		ShareNet:  true,
		Workspace: WorkspaceSpec{Path: "/workspace", Mode: WorkspaceRW},
	}
}

func indexOf(items []string, s string) int {
	for i, item := range items {
		if item == s {
			return i
		}
	}
	return -1
}

// Ensure unused import vars don't break compilation on non-test.
var _ = fmt.Sprintf
var _ = os.Getpid
var _ = uidPtr
