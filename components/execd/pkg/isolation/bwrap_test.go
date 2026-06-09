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

package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
			"--overlay-src /workspace",
			"--overlay /var/lib/execd/isolation/abc",
			"--workdir /var/lib/execd/isolation/abc-work",
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
		if !strings.Contains(s, "--overlay-src /workspace") {
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
		if !strings.Contains(s, "--init-groups") {
			t.Error("missing --init-groups")
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

func TestBuildArgv_Seccomp(t *testing.T) {
	opts := basicWrapOpts()
	argv, err := buildArgv(opts, "/etc/execd/seccomp.bpf")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(argv, " ")
	if !strings.Contains(s, "/etc/execd/seccomp.bpf") {
		t.Error("missing seccomp path")
	}
}

func TestBuildArgv_SegmentOrder(t *testing.T) {
	opts := basicWrapOpts()
	opts.Profile = ProfileStrict
	opts.Workspace.Mode = WorkspaceOverlay
	opts.UpperDir = "/tmp/upper"
	opts.ExtraWritable = []string{"/data"}
	opts.EnvPassthrough = EnvSpec{Mode: EnvModeDeny, Keys: []string{"TOKEN"}}

	argv, err := buildArgv(opts, "/etc/execd/seccomp.bpf")
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
		"--", "setpriv", "--reuid=1000", "--regid=1000", "--init-groups",
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
