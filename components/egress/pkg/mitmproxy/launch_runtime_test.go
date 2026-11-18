// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mitmproxy

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

// Exercise the production argument builder with real mitmdump: an existing
// workload owning either former implicit port must not terminate transparent MITM.
func TestTransparentMitmdumpWithOccupiedExplicitPorts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("transparent mitmdump requires Linux")
	}
	binary, err := exec.LookPath("mitmdump")
	if err != nil {
		t.Skip("mitmdump is not installed")
	}
	t.Setenv(constants.EnvUpstreamProxy, "http://127.0.0.1:3128")
	t.Setenv(constants.EnvExplicitProxyPort, "")
	for _, address := range []string{"127.0.0.1:18082", "127.0.0.1:18083"} {
		listener, err := net.Listen("tcp4", address)
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	cfg := Config{ListenPort: port}
	cfg.ExplicitProxyPort, err = explicitProxyPortFromEnv(cfg)
	require.NoError(t, err)
	args := buildMitmdumpArgs(cfg)
	for i, arg := range args {
		if arg == systemScriptPath || arg == upstreamProxyScriptPath {
			args[i], err = filepath.Abs(filepath.Join("../../mitmscripts", filepath.Base(arg)))
			require.NoError(t, err)
		}
	}
	args = append(args, "--set", "confdir="+t.TempDir(), "--set", "connection_strategy=lazy")
	cmd := exec.Command(binary, args...)
	cmd.Env = os.Environ()
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		t.Log(output.String())
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp4", listener.Addr().String(), 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		require.True(t, time.Now().Before(deadline), "transparent listener did not become ready")
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case err := <-done:
		done <- err // cleanup still reaps the result before reading output
		t.Fatalf("mitmdump exited with occupied workload ports: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}
