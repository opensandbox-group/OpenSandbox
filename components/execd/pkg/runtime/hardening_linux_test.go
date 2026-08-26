//go:build linux

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

package runtime

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

var (
	launcherOnce  sync.Once
	launcherBuilt string
	launcherErr   error
)

func buildLauncher(t *testing.T) string {
	t.Helper()
	launcherOnce.Do(func() {
		cc, err := exec.LookPath("cc")
		if err != nil {
			launcherErr = err
			return
		}
		dir, err := os.MkdirTemp("", "launcher-test-*")
		if err != nil {
			launcherErr = err
			return
		}
		launcherBuilt = filepath.Join(dir, "opensandbox-launcher")
		src := filepath.Join("..", "..", "native", "launcher.c")
		cmd := exec.Command(cc, "-O2", "-Wall", "-Wextra", "-Werror",
			"-o", launcherBuilt, src)
		if out, err := cmd.CombinedOutput(); err != nil {
			launcherErr = err
			t.Logf("launcher build output: %s", out)
		}
	})
	if launcherErr != nil {
		t.Skipf("cannot build opensandbox-launcher: %v", launcherErr)
	}
	return launcherBuilt
}

func resetHardening() {
	hardening.enabled.Store(false)
	hardening.launcherPath = ""
	hardening.policy = nil
	hardening.capDrop.Store(nil)
	hardening.seccomp.Store(nil)
	hardening.landlock.Store(nil)
	launcherSearchPaths = []string{launcherRuntimePath}
}

func initHardeningForTest(t *testing.T, cfg isolation.Config) {
	t.Helper()
	t.Cleanup(resetHardening)
	if err := InitHardening(cfg); err != nil {
		t.Fatalf("InitHardening: %v", err)
	}
}

func hardenedCfg(keepCaps ...string) isolation.Config {
	return isolation.Config{
		Hardening: &isolation.HardeningConfig{
			Enabled:          true,
			KeepCapabilities: keepCaps,
		},
	}
}

// childStatus launches a command through the floor and returns its combined
// output. The command may exit non-zero (e.g. a seccomp-denied syscall);
// assertions run against the output.
func childStatus(t *testing.T, cfg isolation.Config, script string) string {
	t.Helper()
	initHardeningForTest(t, cfg)
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = &out
	cmd.Stderr = &out
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Wait(); err != nil {
		t.Logf("command exited with error: %v", err)
	}
	return out.String()
}

func TestHardeningDisabledByDefault(t *testing.T) {
	initHardeningForTest(t, isolation.Config{})
	report := ReportHardening()
	if report.CapDrop.State != "disabled" || report.Seccomp.State != "disabled" {
		t.Fatalf("hardening states = %q/%q, want disabled/disabled",
			report.CapDrop.State, report.Seccomp.State)
	}
	out := childStatus(t, isolation.Config{}, "echo hi")
	if out != "hi\n" {
		t.Fatalf("output = %q, want hi (launch must be unmodified)", out)
	}
}

func TestHardeningAppliesFloor(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	out := childStatus(t, hardenedCfg(), `grep -E "^CapEff:|^CapPrm:|^NoNewPrivs:|^Uid:" /proc/self/status`)
	if !strings.Contains(out, "NoNewPrivs:	1") {
		t.Fatalf("NoNewPrivs not set: %q", out)
	}
	if strings.Contains(out, "CapEff:	0000000000000000") || os.Geteuid() != 0 {
		return
	}
	t.Fatalf("CapEff not dropped to zero: %q", out)
}

func TestHardeningKeepsExecdPrivileges(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)

	before, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	out := childStatus(t, hardenedCfg(), "true")
	after, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	capOf := func(status []byte) string {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "CapEff:") {
				return line
			}
		}
		return ""
	}
	if capOf(before) != capOf(after) {
		t.Fatalf("execd CapEff changed across a hardened launch:\n before=%s\n after =%s\n child=%q",
			capOf(before), capOf(after), out)
	}
}

func TestHardeningSeccompBlocksDeniedSyscall(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	// The filter is installed (Seccomp: 2 = SECCOMP_MODE_FILTER) before the
	// workload execs; a behavioral probe is unreliable because the container
	// runtime's own seccomp profile already blocks syscalls like mount.
	out := childStatus(t, hardenedCfg(), `grep "^Seccomp:" /proc/self/status`)
	if !strings.Contains(out, "Seccomp:	2") {
		t.Fatalf("seccomp filter not active in the child: %q", out)
	}
}

func TestHardeningEnvStrip(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	initHardeningForTest(t, hardenedCfg())

	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "env")
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = append(os.Environ(),
		"EXECD_ACCESS_TOKEN=supersecret",
		"JUPYTER_TOKEN=anothersecret",
	)
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	_ = mp.Wait()
	for _, secret := range []string{"supersecret", "anothersecret"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("execd credential env leaked into the workload: %q", out.String())
		}
	}
}

func TestHardeningKeepCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to raise capabilities")
	}
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	out := childStatus(t, hardenedCfg("CAP_NET_RAW"), `grep "^CapEff:" /proc/self/status`)
	// CAP_NET_RAW = 13 → 0x2000.
	if !strings.Contains(out, "CapEff:	0000000000002000") {
		t.Fatalf("kept capability not raised: %q", out)
	}
}

func TestHardeningRejectsReservedExecve(t *testing.T) {
	cfg := isolation.Config{
		Seccomp: &isolation.SeccompOverride{Deny: []string{"execve", "mount"}},
		Hardening: &isolation.HardeningConfig{
			Enabled: true,
		},
	}
	if err := InitHardening(cfg); err == nil || !strings.Contains(err.Error(), "execve") {
		t.Fatalf("InitHardening error = %v, want execve rejection", err)
	}
	resetHardening()
}

func TestHardeningRejectsUnknownCapability(t *testing.T) {
	cfg := isolation.Config{
		Hardening: &isolation.HardeningConfig{
			Enabled:          true,
			KeepCapabilities: []string{"CAP_DOES_NOT_EXIST"},
		},
	}
	if err := InitHardening(cfg); err == nil {
		t.Fatal("InitHardening error = nil, want unknown capability rejection")
	}
	resetHardening()
}

func TestHardeningDegradesWhenLauncherMissing(t *testing.T) {
	launcherSearchPaths = nil
	initHardeningForTest(t, hardenedCfg())
	report := ReportHardening()
	if report.CapDrop.State != "degraded" || report.Seccomp.State != "degraded" {
		t.Fatalf("states = %q/%q, want degraded/degraded",
			report.CapDrop.State, report.Seccomp.State)
	}
	// Fail-open: the launch still works without the floor.
	out := childStatus(t, hardenedCfg(), "echo still-works")
	if out != "still-works\n" {
		t.Fatalf("output = %q, want still-works", out)
	}
}

func TestHardeningReportLayers(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	startReaperForTest(t)
	initHardeningForTest(t, hardenedCfg())
	report := ReportHardening()
	if report.Ebpf.State != "disabled" {
		t.Fatalf("ebpf layer = %q, want disabled", report.Ebpf.State)
	}
	if report.CapDrop.State != "active" && report.CapDrop.State != "degraded" {
		t.Fatalf("cap_drop state = %q, want active or degraded (root)", report.CapDrop.State)
	}
	if report.Seccomp.State != "active" {
		t.Fatalf("seccomp state = %q, want active", report.Seccomp.State)
	}
}

func TestHardeningReportDegradesWithoutInitMode(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)

	// Without init topology the entrypoint and /code kernels never pass
	// through the launcher: enabled layers must report degraded, unconfigured
	// ones stay disabled.
	initHardeningForTest(t, hardenedCfg())
	report := ReportHardening()
	if report.InitMode != "none" {
		t.Fatalf("InitMode = %q, want none (no reaper in this test)", report.InitMode)
	}
	for _, layer := range []struct{ name, state string }{
		{"cap_drop", report.CapDrop.State},
		{"seccomp", report.Seccomp.State},
	} {
		if layer.state != "degraded" {
			t.Fatalf("%s state = %q, want degraded (hardening enabled without init mode)", layer.name, layer.state)
		}
	}
	if report.Landlock.State != "disabled" {
		t.Fatalf("landlock state = %q, want disabled (not configured)", report.Landlock.State)
	}
	if !strings.Contains(report.CapDrop.Message, "EXECD_INIT") {
		t.Fatalf("cap_drop message = %q, want EXECD_INIT guidance", report.CapDrop.Message)
	}

	// An enabled Landlock layer must be degraded too, regardless of the
	// underlying kernel state.
	resetHardening()
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	cfg := isolation.DefaultConfig()
	cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
	cfg.Landlock = &isolation.LandlockConfig{Enabled: true}
	initHardeningForTest(t, cfg)
	report = ReportHardening()
	if report.Landlock.State != "degraded" {
		t.Fatalf("landlock state = %q, want degraded (enabled without init mode)", report.Landlock.State)
	}
	if !strings.Contains(report.Landlock.Message, "EXECD_INIT") {
		t.Fatalf("landlock message = %q, want EXECD_INIT guidance", report.Landlock.Message)
	}
}

func TestLandlockDisabledByDefault(t *testing.T) {
	initHardeningForTest(t, isolation.Config{})
	if report := ReportHardening(); report.Landlock.State != "disabled" {
		t.Fatalf("landlock state = %q, want disabled", report.Landlock.State)
	}
}

func TestLandlockActiveOrUnsupported(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	startReaperForTest(t)
	cfg := isolation.DefaultConfig()
	cfg.Hardening = &isolation.HardeningConfig{Enabled: true}
	cfg.Landlock = &isolation.LandlockConfig{
		Enabled:       true,
		ExtraWritable: []string{"/cache"},
		ExtraReadable: []string{"/opt/data"},
	}
	initHardeningForTest(t, cfg)

	report := ReportHardening()
	switch report.Landlock.State {
	case "active":
		if report.Landlock.Message == "" {
			t.Fatalf("landlock active but no message")
		}
		rules := buildLandlockRules(cfg)
		assertLandlockRule(t, rules, "/", llExecute)
		assertLandlockRule(t, rules, "/usr", llReadFile|llReadDir|llExecute)
		assertLandlockRule(t, rules, "/proc/self", llReadFile|llReadDir|llExecute)
		assertLandlockRule(t, rules, "/tmp", llRwAccess)
		// allowed_writable paths carry execute on the default rule: the
		// mount-expansion rule is the backup, not the only source of the
		// workspace exec grant.
		assertLandlockRule(t, rules, "/workspace", llRwAccess|llExecute)
		assertLandlockRule(t, rules, "/cache", llRwAccess)
		assertLandlockRule(t, rules, "/opt/data", llReadFile|llReadDir|llExecute)
		for _, rule := range rules {
			if rule.Path == "/proc" && rule.Access&llReadFile != 0 {
				t.Fatalf("all of /proc must not be granted read access: %+v", rule)
			}
		}
	case "degraded":
		if !strings.Contains(report.Landlock.Message, "/cache") ||
			!strings.Contains(report.Landlock.Message, "/opt/data") {
			t.Fatalf("landlock degraded message = %q, want the missing extra paths", report.Landlock.Message)
		}
	case "unsupported":
		t.Logf("landlock unsupported on this kernel; skipping rule assertions")
	default:
		t.Fatalf("landlock state = %q, want active, degraded or unsupported", report.Landlock.State)
	}
}

func TestPathBeneath(t *testing.T) {
	tests := []struct {
		parent, path string
		want         bool
	}{
		{"/", "/mnt/test/hardened.sh", true},
		{"/mnt", "/mnt/test", true},
		{"/mnt", "/mnt/test/hardened.sh", true},
		{"/mnt", "/mnt", true},
		{"/mnt", "/mntx", false},
		{"/mnt", "/", false},
		{"/usr", "/usr/bin/bash", true},
	}
	for _, tt := range tests {
		if got := pathBeneath(tt.parent, tt.path); got != tt.want {
			t.Fatalf("pathBeneath(%q, %q) = %v, want %v", tt.parent, tt.path, got, tt.want)
		}
	}
}

func TestRuleForPathMergesMatches(t *testing.T) {
	rules := []landlockRule{
		{Access: llExecute, Path: "/"},
		{Access: llRwAccess, Path: "/mnt"},
	}
	// A bind-mounted workspace beneath /mnt must keep execute access from
	// the "/" rule merged with the writable grant.
	access, ok := ruleForPath(rules, "/mnt/test/hardened.sh")
	if !ok || access != llRwAccess|llExecute {
		t.Fatalf("ruleForPath(/mnt/test/hardened.sh) = %#x/%v, want rw+exec", access, ok)
	}
	access, ok = ruleForPath(rules, "/etc/passwd")
	if !ok || access != llExecute {
		t.Fatalf("ruleForPath(/etc/passwd) = %#x/%v, want llExecute", access, ok)
	}
	if _, ok := ruleForPath(rules, "relative"); ok {
		t.Fatal("ruleForPath accepted a relative path")
	}
}

func TestDecodeMountPath(t *testing.T) {
	if got := decodeMountPath(`/mnt/test\040dir`); got != "/mnt/test dir" {
		t.Fatalf("decode = %q", got)
	}
	if got := decodeMountPath(`/a\134b`); got != `a\\b` && got != `/a\b` {
		t.Fatalf("decode backslash = %q", got)
	}
	if got := decodeMountPath("/plain"); got != "/plain" {
		t.Fatalf("decode plain = %q", got)
	}
}

func assertLandlockRule(t *testing.T, rules []landlockRule, path string, access uint64) {
	t.Helper()
	for _, rule := range rules {
		if rule.Path == path {
			if rule.Access != access {
				t.Fatalf("landlock rule %s access = %#x, want %#x", path, rule.Access, access)
			}
			return
		}
	}
	t.Fatalf("landlock rule for %s missing", path)
}

// TestHardeningPTYSessions verifies StartPTY/StartPipe route through the
// launcher (OSEP-0018 R-n): the argv[0]-replacement execve must preserve the
// pty/session semantics (setsid/Setctty, pty fds) while the floor applies,
// with the reaper dispatching launcher-exec'd children.
func TestHardeningPTYSessions(t *testing.T) {
	buildLauncher(t)
	launcherSearchPaths = append(launcherSearchPaths, launcherBuilt)
	startReaperForTest(t)
	initHardeningForTest(t, hardenedCfg())
	requireBash(t)

	// Seed the credential env so the session would leak it without the strip.
	t.Setenv("EXECD_ACCESS_TOKEN", "pty-session-secret")

	// The session shell is the launcher-exec'd workload: read the floor from
	// its own /proc/self/status and verify the credential env was stripped.
	probe := "grep -E '^CapEff:|^Seccomp:|^NoNewPrivs:' /proc/self/status; " +
		"if env | grep -q '^EXECD_ACCESS_TOKEN='; then echo token_leaked; else echo token_stripped; fi"

	assertFloor := func(mode string, data string) {
		t.Helper()
		for _, want := range []string{
			"Seccomp:\t2",
			"NoNewPrivs:\t1",
			"token_stripped",
		} {
			if !strings.Contains(data, want) {
				t.Fatalf("%s session output missing %q:\n%s", mode, want, data)
			}
		}
		// CapEff is only meaningful to assert when execd runs as root (the
		// launcher drops caps it holds; a non-root test process has none).
		if os.Geteuid() == 0 && !strings.Contains(data, "CapEff:\t0000000000000000") {
			t.Fatalf("%s session CapEff not dropped:\n%s", mode, data)
		}
	}

	t.Run("pipe", func(t *testing.T) {
		s := newPTYSession(uuidString(), "", probe)
		if err := s.StartPipe(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.close() })

		if !replayContains(t, s, "token_stripped", 10*time.Second) {
			t.Fatal("pipe session did not produce the floor probe output")
		}
		data, _ := s.replay.ReadFrom(0)
		assertFloor("pipe", string(data))
	})

	t.Run("pty", func(t *testing.T) {
		s := newPTYSession(uuidString(), "", probe)
		if err := s.StartPTY(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.close() })

		if !replayContains(t, s, "token_stripped", 10*time.Second) {
			t.Fatal("pty session did not produce the floor probe output")
		}
		data, _ := s.replay.ReadFrom(0)
		assertFloor("pty", string(data))
	})
}
