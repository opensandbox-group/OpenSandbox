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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestDropTelemetryPeer(t *testing.T) {
	ready := os.Getenv("OPENSANDBOX_DROP_PEER_READY")
	if ready == "" {
		return
	}
	for _, port := range []string{"15431", "15433"} {
		listener, err := net.Listen("tcp4", ":"+port)
		require.NoError(t, err)
		defer listener.Close()
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
			}
		}()
	}
	require.NoError(t, os.WriteFile(ready, nil, 0600))
	<-t.Context().Done()
}

func TestDropTelemetrySidecarDataPlaneKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if !privateDropNetNS(t, ctx) {
		return
	}
	ready := filepath.Join(t.TempDir(), "peer-ready")
	peer := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestDropTelemetryPeer$", "-test.timeout=40s")
	peer.Env = append(os.Environ(), "OPENSANDBOX_DROP_PEER_READY="+ready)
	peer.Stdout, peer.Stderr = os.Stdout, os.Stderr
	require.NoError(t, peer.Start())
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 4*time.Second, 10*time.Millisecond)
	pid := strconv.Itoa(peer.Process.Pid)
	// Both ends are private subprocess netns, matching the nftables peer fixtures.
	for _, cmd := range [][]string{
		{"ip", "link", "add", "client0", "type", "veth", "peer", "name", "origin0", "netns", pid},
		{"ip", "addr", "add", "198.18.0.1/29", "dev", "client0"},
		{"ip", "link", "set", "client0", "up"},
		{"nsenter", "-t", pid, "-n", "ip", "addr", "add", "198.18.0.2/29", "dev", "origin0"},
		{"nsenter", "-t", pid, "-n", "ip", "addr", "add", "198.18.0.3/29", "dev", "origin0"},
		{"nsenter", "-t", pid, "-n", "ip", "link", "set", "origin0", "up"},
	} {
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		require.NoError(t, err, "%v: %s", cmd, out)
	}
	control, err := net.DialTimeout("tcp4", "198.18.0.2:15433", time.Second)
	require.NoError(t, err)
	require.NoError(t, control.SetDeadline(time.Now().Add(time.Second)))
	_, err = control.Write([]byte{1})
	require.NoError(t, err)
	_, err = io.ReadFull(control, make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, control.Close())
	cfg := droptelemetry.DefaultConfig()
	cfg.Mode = droptelemetry.Sample
	cfg.Poll = time.Hour
	rt := droptelemetry.New(cfg, nftables.TableName)
	requireNFLOG(t, ctx, cfg, rt, func(droptelemetry.Packet) {})
	pol, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"deny","target":"198.18.0.2","ports":[15431]},{"action":"deny","target":"198.18.0.3"}]}`)
	require.NoError(t, err)
	require.NoError(t, nftables.NewManagerWithOptions(nftables.Options{DropTelemetry: rt}).ApplyStatic(ctx, pol))
	syn := "ip daddr 198.18.0.2 tcp dport 15431 tcp flags & (syn | ack) == syn"
	udp := "ip daddr 198.18.0.3 udp dport 15432"
	var probe string
	probe = "add table inet drop_sidecar_probe\n"
	for _, name := range []string{"before_syn", "before_udp", "after_syn", "after_udp"} {
		probe += "add counter inet drop_sidecar_probe " + name + "\n"
	}
	for _, hook := range []struct {
		name     string
		priority int
	}{{"before", -1}, {"after", 1}} {
		probe += fmt.Sprintf("add chain inet drop_sidecar_probe %s { type filter hook output priority %d; policy accept; }\n", hook.name, hook.priority)
		probe += fmt.Sprintf("add rule inet drop_sidecar_probe %s %s counter name %s_syn\nadd rule inet drop_sidecar_probe %s %s counter name %s_udp\n", hook.name, syn, hook.name, hook.name, udp, hook.name)
	}
	out, err := runKernelNft(ctx, probe)
	require.NoError(t, err, "%s", out)
	conn, err := net.DialTimeout("tcp4", "198.18.0.2:15431", 80*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	require.Error(t, err, "denied SYN reached its listening peer")
	packet, err := net.Dial("udp4", "198.18.0.3:15432")
	require.NoError(t, err)
	_, err = packet.Write([]byte("blocked"))
	require.True(t, err == nil || errors.Is(err, syscall.EPERM), "%v", err)
	packet.Close()
	out, err = exec.CommandContext(ctx, "nft", "-j", "list", "counters", "table", "inet", nftables.TableName).CombinedOutput()
	require.NoError(t, err, "%s", out)
	counts, err := droptelemetry.ParseCounters(out)
	require.NoError(t, err)
	require.EqualValues(t, 1, counts["port_deny"])
	require.EqualValues(t, 1, counts["deny_set"])
	out, err = exec.CommandContext(ctx, "nft", "-j", "list", "counters", "table", "inet", "drop_sidecar_probe").CombinedOutput()
	require.NoError(t, err, "%s", out)
	var doc struct {
		Nftables []struct {
			Counter struct {
				Name    string
				Packets uint64
			}
		}
	}
	require.NoError(t, json.Unmarshal(out, &doc))
	observed := map[string]uint64{}
	for _, item := range doc.Nftables {
		if item.Counter.Name != "" {
			observed[item.Counter.Name] = item.Counter.Packets
		}
	}
	require.Equal(t, map[string]uint64{"before_syn": 1, "before_udp": 1, "after_syn": 0, "after_udp": 0}, observed)
	t.Logf("real sidecar policy counts=%v; hook probes=%v", counts, observed)
}
