// Copyright 2026 The OpenSandbox Authors
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

package droptelemetry_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
)

func privateDropNetNS(t *testing.T, ctx context.Context) bool {
	t.Helper()
	if os.Getenv("OPENSANDBOX_NFT_TEST") != "1" {
		t.Skip("kernel validation not executed: set OPENSANDBOX_NFT_TEST=1")
	}
	parent := os.Getenv("OPENSANDBOX_DROP_PARENT_NETNS")
	current, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)
	if parent != "" {
		require.NotEqual(t, parent, current)
		t.Logf("private netns %s, parent %s", current, parent)
		return true
	}
	cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "OPENSANDBOX_DROP_PARENT_NETNS="+current)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Logf("%s", out)
	if strings.Contains(string(out), "NFLOG_UNAVAILABLE:") {
		t.Skip("NFLOG_UNAVAILABLE: see private-netns output")
	}
	return false
}
func runKernelNft(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}
func requireNFLOG(t *testing.T, ctx context.Context, cfg droptelemetry.Config, rt *droptelemetry.Runtime, onPacket func(droptelemetry.Packet)) func() {
	t.Helper()
	var bindErr, probeErr error
	closeNF := rt.Start(ctx, func(ctx context.Context, s string) ([]byte, error) {
		out, e := runKernelNft(ctx, s)
		probeErr = e
		return out, e
	}, func(ctx context.Context, c droptelemetry.Config, packet func(droptelemetry.Packet), failure func(error)) (func(), error) {
		closeNF, e := droptelemetry.Bind(ctx, c, packet, failure)
		bindErr = e
		return closeNF, e
	}, onPacket)
	t.Cleanup(closeNF)
	if bindErr != nil {
		msg := strings.ToLower(bindErr.Error())
		if strings.Contains(msg, "protocol not supported") || strings.Contains(msg, "operation not supported") || strings.Contains(msg, "no such file or directory") {
			t.Skipf("NFLOG_UNAVAILABLE: %v", bindErr)
		}
		require.NoError(t, bindErr)
	}
	require.NoError(t, probeErr)
	return closeNF
}
func TestDropTelemetryKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if !privateDropNetNS(t, ctx) {
		return
	}
	out, err := exec.CommandContext(ctx, "ip", "link", "set", "lo", "up").CombinedOutput()
	require.NoError(t, err, "%s", out)
	cfg := droptelemetry.DefaultConfig()
	cfg.Mode = droptelemetry.Sample
	cfg.Rate = "1/minute"
	cfg.Burst = 1
	cfg.Poll = time.Hour
	rt := droptelemetry.New(cfg, "drop_test")
	samples := make(chan droptelemetry.Packet, 64)
	requireNFLOG(t, ctx, cfg, rt, func(p droptelemetry.Packet) {
		select {
		case samples <- p:
		default:
		}
	})
	run := rt.Wrap(runKernelNft, func(line string) droptelemetry.Reason {
		if strings.Contains(line, "udp dport") {
			return droptelemetry.DoT
		}
		if strings.Contains(line, "tcp dport") {
			return droptelemetry.PortDeny
		}
		return droptelemetry.DenySet
	})
	_, err = run(ctx, "add table inet drop_test\nadd chain inet drop_test output { type filter hook output priority 0; policy accept; }\nadd rule inet drop_test output udp dport 15354 drop\nadd rule inet drop_test output tcp dport 15355 drop\nadd rule inet drop_test output ip daddr 127.0.0.2 drop\n")
	require.NoError(t, err)
	udp, err := net.ListenPacket("udp4", "127.0.0.1:15354")
	require.NoError(t, err)
	defer udp.Close()
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 15355})
	require.NoError(t, err)
	defer tcp.Close()
	for i := 0; i < 8; i++ {
		conn, e := net.Dial("udp4", "127.0.0.1:15354")
		require.NoError(t, e)
		_, e = conn.Write([]byte("drop test"))
		require.True(t, e == nil || errors.Is(e, syscall.EPERM), "%v", e)
		conn.Close()
	}
	for i := 0; i < 3; i++ {
		conn, e := net.DialTimeout("tcp4", "127.0.0.1:15355", 80*time.Millisecond)
		if conn != nil {
			conn.Close()
		}
		require.Error(t, e, "SYN must still be dropped after sample limit is exhausted")
	}
	conn, err := net.Dial("udp4", "127.0.0.2:15356")
	require.NoError(t, err)
	_, err = conn.Write([]byte("address-only drop"))
	require.True(t, err == nil || errors.Is(err, syscall.EPERM), "%v", err)
	conn.Close()
	seen := map[string]int{}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
collect:
	for {
		select {
		case p := <-samples:
			seen[string(p.Reason)+":"+p.Proto]++
			require.True(t, p.SrcIP.IsLoopback())
		case <-deadline.C:
			break collect
		}
	}
	require.Equal(t, 1, seen["dot:udp"])
	require.Equal(t, 1, seen["port_deny:tcp"])
	require.Equal(t, 1, seen["deny_set:udp"])
	require.NoError(t, udp.SetReadDeadline(time.Now().Add(50*time.Millisecond)))
	_, _, err = udp.ReadFrom(make([]byte, 32))
	require.Error(t, err, "even UDP packets above the sample limit remain dropped")
	require.NoError(t, tcp.SetDeadline(time.Now().Add(50*time.Millisecond)))
	accepted, err := tcp.Accept()
	if accepted != nil {
		accepted.Close()
	}
	require.Error(t, err)
	data, err := exec.CommandContext(ctx, "nft", "-j", "list", "counters", "table", "inet", "drop_test").CombinedOutput()
	require.NoError(t, err, "%s", data)
	counts, err := droptelemetry.ParseCounters(data)
	require.NoError(t, err)
	require.EqualValues(t, 8, counts["dot"])
	require.GreaterOrEqual(t, counts["port_deny"], uint64(3))
	require.EqualValues(t, 1, counts["deny_set"])
	t.Logf("samples=%v counters=%v; over-limit TCP/UDP delivery blocked", seen, counts)
}
func TestDropTelemetryBindCloseKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !privateDropNetNS(t, ctx) {
		return
	}
	cfg := droptelemetry.DefaultConfig()
	var callbacks atomic.Int64
	closeNF, err := droptelemetry.Bind(ctx, cfg, func(droptelemetry.Packet) {}, func(error) { callbacks.Add(1) })
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "protocol not supported") || strings.Contains(msg, "operation not supported") || strings.Contains(msg, "no such file or directory") {
			t.Skipf("NFLOG_UNAVAILABLE: %v", err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(closeNF)
	closed := make(chan struct{})
	go func() { closeNF(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		cancel()
		<-closed
		t.Fatal("NFLOG close blocked while the parent context was live")
	}
	require.NoError(t, ctx.Err(), "closing the receiver must not cancel its parent")
	require.Zero(t, callbacks.Load(), "socket closure must not invoke the receive error callback")
	time.Sleep(50 * time.Millisecond)
	require.Zero(t, callbacks.Load(), "closed receiver must not spin on receive errors")
	closeNF()
	t.Log("receiver closed with a live parent context; no error callback loop")
}
