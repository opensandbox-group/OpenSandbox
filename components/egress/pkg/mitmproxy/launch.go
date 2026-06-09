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
	"fmt"
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

// Config: mitmdump --mode transparent. Static options (mode, connection_strategy,
// listen_host, stream_large_bodies, ignore_hosts,
// ssl_verify_upstream_trusted_confdir) live in
// /var/lib/mitmproxy/.mitmproxy/config.yaml and are auto-loaded by mitmdump.
// This struct carries only per-launch dynamic values that override those
// defaults via `--set`.
type Config struct {
	ListenPort int
	UserName   string
	// ScriptPath is an optional user-supplied addon, loaded after the system addon.
	ScriptPath string
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

	// Only per-launch dynamic values are passed on the command line. Static
	// options (mode, listen_host, connection_strategy, stream_large_bodies,
	// http2, ignore_hosts, ssl_verify_upstream_trusted_confdir) come from
	// /var/lib/mitmproxy/.mitmproxy/config.yaml shipped in the egress image.
	// `--set` overrides config.yaml, so the env-driven overrides below take
	// precedence at runtime without rebuilding the image.
	args := []string{
		"--listen-port", strconv.Itoa(cfg.ListenPort),
	}

	// Upstream cert trust path override. Default in config.yaml is /etc/ssl/certs;
	// override per-deployment when the upstream uses a private CA bundle.
	if trustDir := strings.TrimSpace(os.Getenv(constants.EnvMitmproxyUpstreamTrustDir)); trustDir != "" {
		args = append(args, "--set", "ssl_verify_upstream_trusted_confdir="+trustDir)
	}

	// Transparent mode redirects TCP to IP addresses. Clients connecting to IPs
	// do not send SNI, so upstream TLS cert hostname verification fails with
	// "IP address mismatch". Set OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true
	// to skip upstream verification when clients connect by IP.
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxySslInsecure)) {
		args = append(args, "--set", "ssl_insecure=true")
	}

	// Load the system addon first so user addons can observe / override its hooks.
	args = append(args, "-s", systemScriptPath)
	if user := strings.TrimSpace(cfg.ScriptPath); user != "" {
		args = append(args, "-s", user)
	}

	cmd := exec.Command("mitmdump", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	// HOME determines mitm's confdir (~/.mitmproxy) which holds both the CA
	// and the baked-in config.yaml. Env override takes precedence when a volume
	// mount shadows the default path.
	homeEnv := "HOME=" + home
	if confdir := strings.TrimSpace(os.Getenv(constants.EnvMitmproxyConfdir)); confdir != "" {
		args = append(args, "--set", "confdir="+confdir)
	}
	cmd.Env = append(os.Environ(), homeEnv)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mitmproxy: start mitmdump: %w", err)
	}
	done := make(chan error, 1)
	onExit := cfg.OnExit
	safego.Go(func() {
		err := cmd.Wait()
		done <- err
		if onExit != nil {
			onExit(err)
		}
	})

	log.Infof("[mitmproxy] mitmdump started (pid %d, transparent on %s:%d)", cmd.Process.Pid, listenHostLoopback, cfg.ListenPort)
	return &Running{Cmd: cmd, done: done}, nil
}
