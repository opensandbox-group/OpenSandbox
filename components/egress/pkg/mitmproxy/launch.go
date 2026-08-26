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

package mitmproxy

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/internal/safego"
)

const RunAsUser = "mitmproxy"

// Loopback: transparent mode receives via REDIRECT; do not listen on 0.0.0.0 in the netns.
// Kept as a Go constant only for the startup log line; the actual listen_host is set in
// /var/lib/mitmproxy/.mitmproxy/config.yaml (shipped via the egress Dockerfile).
const listenHostLoopback = "127.0.0.1"

// systemScriptPath: bundled system addon shipped via the egress Dockerfile
// (COPY components/egress/mitmscripts /var/egress/mitmscripts). Always loaded.
const systemScriptPath = "/var/egress/mitmscripts/system.py"

// Config carries only per-launch dynamic values, applied via `--set`. Static
// options (mode, listen_host, connection_strategy, stream_large_bodies,
// ignore_hosts, ssl_verify_upstream_trusted_confdir) are auto-loaded by
// mitmdump from /var/lib/mitmproxy/.mitmproxy/config.yaml (shipped from
// components/egress/mitmproxy/config.yaml).
type Config struct {
	ListenPort int
	UserName   string
	// ScriptPaths are optional user-supplied addons, loaded after the system addon
	// in the order given. Parsed from the comma-separated OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT env var.
	ScriptPaths []string
	// OnExit is called (if non-nil) when mitmdump exits. Called from a background goroutine.
	OnExit func(error)
}

// Running: child mitmdump; use GracefulShutdown to SIGTERM+reap before process exit.
type Running struct {
	Cmd  *exec.Cmd
	done chan error
}

func LookupUser(userName string) (uid, gid uint32, home string, err error) {
	if strings.TrimSpace(userName) == "" {
		userName = RunAsUser
	}
	u, err := user.Lookup(userName)
	if err != nil {
		return 0, 0, "", err
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, "", err
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, "", err
	}
	return uint32(uid64), uint32(gid64), u.HomeDir, nil
}

// Launch starts mitmdump in the background; check Wait/GracefulShutdown on the returned Running.
func Launch(cfg Config) (*Running, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("mitmproxy: transparent mitmdump is only supported on linux")
	}

	if cfg.ListenPort <= 0 {
		return nil, fmt.Errorf("mitmproxy: invalid listen port")
	}
	uname := cfg.UserName
	if strings.TrimSpace(uname) == "" {
		uname = RunAsUser
	}
	uid, gid, home, err := LookupUser(uname)
	if err != nil {
		return nil, fmt.Errorf("mitmproxy: lookup user %q: %w", uname, err)
	}

	args := buildMitmdumpArgs(cfg)

	cmd := exec.Command("mitmdump", args...)
	mitmOut, mitmIn := io.Pipe()
	cmd.Stdout = mitmIn
	cmd.Stderr = mitmIn
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	// HOME determines mitm's confdir (~/.mitmproxy) which holds both the CA
	// and the baked-in config.yaml.
	cmd.Env = buildMitmdumpEnv(os.Environ(), home)

	if err := cmd.Start(); err != nil {
		_ = mitmIn.Close()
		return nil, fmt.Errorf("mitmproxy: start mitmdump: %w", err)
	}
	safego.Go(func() { forwardMitmdumpOutput(mitmOut) })
	done := make(chan error, 1)
	onExit := cfg.OnExit
	safego.Go(func() {
		err := cmd.Wait()
		// cmd.Wait waits for the internal copy from the child to complete, so
		// closing the write end here EOFs the reader only after the remaining
		// mitmdump output has been drained.
		_ = mitmIn.Close()
		done <- err
		if onExit != nil {
			onExit(err)
		}
	})

	log.Infof("[mitmproxy] mitmdump started (pid %d, transparent on %s:%d)", cmd.Process.Pid, listenHostLoopback, cfg.ListenPort)
	return &Running{Cmd: cmd, done: done}, nil
}

func buildMitmdumpArgs(cfg Config) []string {
	args := []string{
		"--listen-port", strconv.Itoa(cfg.ListenPort),
		"--set", "flow_detail=0",
	}

	if trustDir := strings.TrimSpace(os.Getenv(constants.EnvMitmproxyUpstreamTrustDir)); trustDir != "" {
		args = append(args, "--set", "ssl_verify_upstream_trusted_confdir="+trustDir)
	}

	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxySslInsecure)) {
		args = append(args, "--set", "ssl_insecure=true")
	}

	args = append(args, "-s", systemScriptPath)
	for _, p := range cfg.ScriptPaths {
		if s := strings.TrimSpace(p); s != "" {
			args = append(args, "-s", s)
		}
	}
	return args
}

func buildMitmdumpEnv(base []string, home string) []string {
	env := make([]string, 0, len(base)+1)
	env = append(env, base...)
	env = append(env, "HOME="+home)
	return env
}

// forwardMitmdumpOutput relays credential proxy log lines from mitmdump
// stdout/stderr into the egress zap logger at warn level, so they land in the
// same sink as egress logs (OPENSANDBOX_LOG_OUTPUT) and stand out from
// mitmproxy's own high-volume flow logs, which are dropped.
func forwardMitmdumpOutput(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		msg, ok := credentialProxyMessage(line)
		if !ok {
			continue
		}
		log.Warnf("[mitmproxy] %s", msg)
	}
	// On ErrTooLong (a newline-free line over the buffer limit) Scan stops
	// early; closing the read end makes the exec copy goroutine fail with
	// ErrClosedPipe so cmd.Wait returns instead of hanging forever.
	_ = r.Close()
}

// credentialProxyMessage returns the message of a credential proxy log line,
// stripping the leading [HH:MM:SS.mmm] timestamp that mitmproxy 11.x terminal
// logger prepends to ctx.log.* records. It reports false for any other line,
// so mitmproxy's own high-volume flow logs stay filtered out.
func credentialProxyMessage(line string) (string, bool) {
	if strings.HasPrefix(line, "[") {
		if end := strings.Index(line, "] "); end != -1 {
			line = line[end+2:]
		}
	}
	if !strings.HasPrefix(line, "credential proxy:") {
		return "", false
	}
	return line, true
}
