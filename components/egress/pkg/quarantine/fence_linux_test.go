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

package quarantine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// Kernel tests deliberately fail, rather than skip, if explicitly enabled but
// their privileged dependencies are unavailable. The parent namespace is never
// modified. Without OPENSANDBOX_NFT_TEST=1 the real packet tests are unexecuted.
func isolatedFenceKernelTest(t *testing.T, ctx context.Context) bool {
	t.Helper()
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parent, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, "unshare", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=KILL", os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		cmd.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=quarantine-netns", "OPENSANDBOX_FENCE_PARENT_NETNS="+parent)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		t.Logf("%s", out)
		return false
	case "quarantine-netns":
		parent := os.Getenv("OPENSANDBOX_FENCE_PARENT_NETNS")
		require.NotEmpty(t, parent)
		current, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, parent, current)
		return true
	default:
		t.Skip("kernel packet validation not executed: set OPENSANDBOX_NFT_TEST=1 with nft/ip/unshare/nsenter and namespace capabilities")
		return false
	}
}

func TestQuarantineFenceKernelFaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if !isolatedFenceKernelTest(t, ctx) {
		return
	}
	f := NewFence()
	require.NoError(t, f.Ensure(ctx))
	require.NoError(t, f.Ensure(ctx), "replacing an existing fence must be atomic and idempotent")
	cleanupKernelFence(t, ctx)
	injected := errors.New("injected result loss")
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("ensure-after-apply=%v", after), func(t *testing.T) {
			cleanupKernelFence(t, ctx)
			f := &Fence{run: func(ctx context.Context, script string, args ...string) ([]byte, error) {
				if script != "" && !after {
					return nil, injected
				}
				out, err := runFenceNft(ctx, script, args...)
				if script != "" && err == nil {
					return out, injected
				}
				return out, err
			}}
			err := f.Ensure(ctx)
			if after {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			present, err := NewFence().read(ctx)
			require.NoError(t, err)
			require.Equal(t, after, present)
		})
	}
	t.Run("readback-failure", func(t *testing.T) {
		require.NoError(t, NewFence().Ensure(ctx))
		f := &Fence{run: func(ctx context.Context, script string, args ...string) ([]byte, error) {
			if script == "" {
				return nil, injected
			}
			return runFenceNft(ctx, script, args...)
		}}
		require.ErrorIs(t, f.Ensure(ctx), injected)
		present, err := NewFence().read(ctx)
		require.NoError(t, err)
		require.True(t, present)
	})
	t.Run("text-readback-failure", func(t *testing.T) {
		f := &Fence{run: func(ctx context.Context, script string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "-n" {
				return nil, injected
			}
			return runFenceNft(ctx, script, args...)
		}}
		require.ErrorIs(t, f.Ensure(ctx), injected)
		present, err := NewFence().read(ctx)
		require.NoError(t, err)
		require.True(t, present)
	})
	require.NoError(t, NewFence().Ensure(ctx))
	_, err := runFenceNft(ctx, "add table inet opensandbox_quarantine { flags dormant; }\n", "-f", "-")
	require.NoError(t, err)
	_, err = NewFence().read(ctx)
	require.Error(t, err, "a disabled table must never be accepted")
	require.NoError(t, NewFence().Ensure(ctx), "must replace a disabled table")
	// Existing-table updates may ignore userdata, so replace it atomically.
	foreignTable := strings.Replace(fenceExpectedTable, fenceOwner, "foreign", 1)
	_, err = runFenceNft(ctx, "delete table inet opensandbox_quarantine\n"+foreignTable, "-f", "-")
	require.NoError(t, err)
	raw, err := runFenceNft(ctx, "", "-n", "-y", "list", "table", "inet", fenceTable)
	require.NoError(t, err)
	require.Contains(t, string(raw), `comment "foreign"`, "the negative fixture must really change the kernel owner")
	_, err = NewFence().read(ctx)
	require.Error(t, err, "foreign ownership must fail even when old JSON omits table comments")
	require.NoError(t, NewFence().Ensure(ctx))
	cleanupKernelFence(t, ctx)
}

// cleanupKernelFence only releases this test's isolated namespace resources.
// It is deliberately test-only: the runtime has no unfence/recovery operation.
func cleanupKernelFence(t *testing.T, ctx context.Context) {
	t.Helper()
	present, err := NewFence().read(ctx)
	require.NoError(t, err)
	if present {
		_, err = runFenceNft(ctx, "delete table inet opensandbox_quarantine\n", "-f", "-")
		require.NoError(t, err)
	}
	present, err = NewFence().read(ctx)
	require.NoError(t, err)
	require.False(t, present, "test namespace cleanup left its quarantine table")
}

// The traffic matrix exercises all three hooks using veth-connected peers and
// counters at the receiving socket. Failed replies alone are insufficient:
// input could be open while output drops the echo. Every baseline is delivered
// before fencing, and each receiver must remain unchanged while fenced.
func TestQuarantineFenceKernelTraffic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if !isolatedFenceKernelTest(t, ctx) {
		return
	}
	for _, binary := range []string{"nft", "ip", "nsenter", "unshare"} {
		_, err := exec.LookPath(binary)
		require.NoError(t, err)
	}
	kernelCmd(t, ctx, 0, "ip", "link", "set", "lo", "up")
	kernelCmd(t, ctx, 0, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	kernelCmd(t, ctx, 0, "sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
	local := startFencePeer(t, ctx, false)
	first, second := startFencePeer(t, ctx, true), startFencePeer(t, ctx, true)
	for index, peer := range []*fencePeer{first, second} {
		n := index + 1
		link := fmt.Sprintf("q%d", n)
		other := fmt.Sprintf("p%d", n)
		kernelCmd(t, ctx, 0, "ip", "link", "add", link, "type", "veth", "peer", "name", other)
		kernelCmd(t, ctx, 0, "ip", "link", "set", other, "netns", strconv.Itoa(peer.pid))
		kernelCmd(t, ctx, 0, "ip", "addr", "add", fmt.Sprintf("198.18.%d.1/24", n), "dev", link)
		kernelCmd(t, ctx, 0, "ip", "-6", "addr", "add", fmt.Sprintf("2001:db8:%d::1/64", n), "dev", link, "nodad")
		kernelCmd(t, ctx, 0, "ip", "link", "set", link, "up")
		kernelCmd(t, ctx, peer.pid, "ip", "link", "set", "lo", "up")
		kernelCmd(t, ctx, peer.pid, "ip", "addr", "add", fmt.Sprintf("198.18.%d.2/24", n), "dev", other)
		kernelCmd(t, ctx, peer.pid, "ip", "-6", "addr", "add", fmt.Sprintf("2001:db8:%d::2/64", n), "dev", other, "nodad")
		kernelCmd(t, ctx, peer.pid, "ip", "link", "set", other, "up")
		kernelCmd(t, ctx, peer.pid, "ip", "route", "add", "default", "via", fmt.Sprintf("198.18.%d.1", n))
		kernelCmd(t, ctx, peer.pid, "ip", "-6", "route", "add", "default", "via", fmt.Sprintf("2001:db8:%d::1", n))
		rootMAC := fenceLinkMAC(t, ctx, 0, link)
		peerMAC := fenceLinkMAC(t, ctx, peer.pid, other)
		for _, pair := range [][2]string{{fmt.Sprintf("198.18.%d.1", n), fmt.Sprintf("198.18.%d.2", n)}, {fmt.Sprintf("2001:db8:%d::1", n), fmt.Sprintf("2001:db8:%d::2", n)}} {
			kernelCmd(t, ctx, 0, "ip", "neigh", "replace", pair[1], "lladdr", peerMAC, "nud", "permanent", "dev", link)
			kernelCmd(t, ctx, peer.pid, "ip", "neigh", "replace", pair[0], "lladdr", rootMAC, "nud", "permanent", "dev", other)
		}
	}
	// Dedicated upstream addresses are denied to ordinary workloads. Only the
	// production UID-scoped proxy rule permits TCP 3128 before quarantine.
	for _, args := range [][]string{{"addr", "add", "198.18.1.3/24", "dev", "p1"}, {"-6", "addr", "add", "2001:db8:1::3/64", "dev", "p1", "nodad"}} {
		kernelCmd(t, ctx, first.pid, "ip", args...)
	}
	peerMAC := fenceLinkMAC(t, ctx, first.pid, "p1")
	for _, address := range []string{"198.18.1.3", "2001:db8:1::3"} {
		kernelCmd(t, ctx, 0, "ip", "neigh", "replace", address, "lladdr", peerMAC, "nud", "permanent", "dev", "q1")
	}
	mitm := startFencePeer(t, ctx, false)
	uid := mitm.call(t, fencePeerRequest{Op: "uid", UID: 10042})
	require.Empty(t, uid.Error)
	require.Equal(t, 10042, uid.UID)
	manager := nftables.NewManagerWithOptions(nftables.Options{UpstreamProxy: &nftables.UpstreamProxyEndpoint{Port: 3128, UID: 10042, IPs: []netip.Addr{netip.MustParseAddr("198.18.1.3"), netip.MustParseAddr("2001:db8:1::3")}}})
	allow, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"deny","target":"198.18.1.3"},{"action":"deny","target":"2001:db8:1::3"}]}`)
	require.NoError(t, err)
	require.NoError(t, manager.ApplyStatic(ctx, allow))
	type flow struct {
		name, network, address, listener string
		client, server                   *fencePeer
		mark                             int
	}
	var flows []flow
	for _, version := range []string{"4", "6"} {
		outside, inside, forward, loop, upstream := "198.18.1.2", "198.18.1.1", "198.18.2.2", "127.0.0.1", "198.18.1.3"
		if version == "6" {
			outside, inside, forward, loop, upstream = "2001:db8:1::2", "2001:db8:1::1", "2001:db8:2::2", "::1", "2001:db8:1::3"
		}
		for _, protocol := range []string{"tcp", "udp"} {
			for _, spec := range []struct {
				name, address  string
				client, server *fencePeer
				mark           int
			}{
				{"output", outside, local, first, 0}, {"marked-output", outside, local, first, constants.MarkValue},
				{"input", inside, first, local, 0}, {"forward", forward, first, second, 0},
				{"loopback", loop, local, local, 0}, {"mitm-uid-output", outside, mitm, first, 0}, {"mitm-uid-loopback", loop, mitm, local, 0},
			} {
				name := protocol + version + "-" + spec.name
				response := spec.server.call(t, fencePeerRequest{Op: "listen", ID: name, Network: protocol + version, Address: net.JoinHostPort(spec.address, "0")})
				require.Empty(t, response.Error)
				flows = append(flows, flow{name, protocol + version, response.Address, name, spec.client, spec.server, spec.mark})
			}
		}
		name := "tcp" + version + "-upstream-proxy"
		response := first.call(t, fencePeerRequest{Op: "listen", ID: name, Network: "tcp" + version, Address: net.JoinHostPort(upstream, "3128")})
		require.Empty(t, response.Error)
		denied := local.call(t, fencePeerRequest{Op: "dial", ID: "denied-upstream-" + version, Network: "tcp" + version, Address: response.Address})
		require.NotEmpty(t, denied.Error, "ordinary UID must not have upstream exception")
		flows = append(flows, flow{name, "tcp" + version, response.Address, name, mitm, first, 0})
	}
	for _, flow := range flows {
		require.Empty(t, flow.client.call(t, fencePeerRequest{Op: "dial", ID: flow.name, Network: flow.network, Address: flow.address, Mark: flow.mark}).Error, flow.name)
		require.Empty(t, flow.client.call(t, fencePeerRequest{Op: "exchange", ID: flow.name, Payload: "before"}).Error, flow.name)
		require.EqualValues(t, 1, flow.server.call(t, fencePeerRequest{Op: "count", ID: flow.listener}).Count, flow.name)
	}
	fence := NewFence()
	require.NoError(t, fence.Ensure(ctx))
	// Normal policy teardown and reapplication cannot remove or replace quarantine.
	require.NoError(t, manager.RemoveEnforcement(ctx))
	present, err := fence.read(ctx)
	require.NoError(t, err)
	require.True(t, present)
	require.NoError(t, manager.ApplyStatic(ctx, allow))
	// Exercise the actual legacy pre-start script in this test's isolated PID
	// namespace as well as its network namespace. Its pkill cannot touch host
	// processes, and deleting the seeded redirect tables proves cleanup ran.
	var redirects strings.Builder
	for _, family := range []string{"inet", "ip", "ip6"} {
		for _, name := range []string{"opensandbox_dns_redirect", "opensandbox_mitm_redirect"} {
			fmt.Fprintf(&redirects, "add table %s %s\n", family, name)
		}
	}
	_, err = runFenceNft(ctx, redirects.String(), "-f", "-")
	require.NoError(t, err)
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	cleanupPath := filepath.Join(filepath.Dir(source), "..", "..", "scripts", "cleanup.sh")
	cleanup := exec.CommandContext(ctx, "sh", cleanupPath)
	cleanup.Env = append(os.Environ(), "OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=")
	output, err := cleanup.CombinedOutput()
	require.NoError(t, err, "%s", output)
	tables, err := runFenceNft(ctx, "", "-j", "list", "tables")
	require.NoError(t, err)
	require.NotContains(t, string(tables), "opensandbox_dns_redirect")
	require.NotContains(t, string(tables), "opensandbox_mitm_redirect")
	present, err = fence.read(ctx)
	require.NoError(t, err)
	require.True(t, present, "legacy cleanup must preserve the independent fence")
	for _, flow := range flows {
		t.Run(flow.name, func(t *testing.T) {
			require.NotEmpty(t, flow.client.call(t, fencePeerRequest{Op: "exchange", ID: flow.name, Payload: "old-while-fenced", TimeoutMS: 250}).Error, "old flow delivered")
			response := flow.client.call(t, fencePeerRequest{Op: "dial", ID: flow.name + "-new", Network: flow.network, Address: flow.address, Mark: flow.mark, TimeoutMS: 250})
			if strings.HasPrefix(flow.network, "tcp") {
				require.NotEmpty(t, response.Error, "new TCP connection established")
			} else {
				require.Empty(t, response.Error)
				require.NotEmpty(t, flow.client.call(t, fencePeerRequest{Op: "exchange", ID: flow.name + "-new", Payload: "new-while-fenced", TimeoutMS: 250}).Error, "new UDP payload delivered")
			}
			require.EqualValues(t, 1, flow.server.call(t, fencePeerRequest{Op: "count", ID: flow.listener}).Count, "receiver saw data while fenced")
		})
	}
	// Check all receivers again after the entire matrix, catching delayed delivery.
	for _, flow := range flows {
		require.EqualValues(t, 1, flow.server.call(t, fencePeerRequest{Op: "count", ID: flow.listener}).Count, flow.name)
	}
	cleanupKernelFence(t, ctx)
	t.Logf("verified %d IPv4/IPv6 TCP/UDP flows: existing and new traffic, input/output/forward, loopback, mark=0x1, MITM UID and scoped upstream proxy", len(flows))
}

// This unprivileged socket test validates the control and receive-counter
// fixture even when real kernel isolation tests cannot be executed locally.
func TestQuarantineFencePeerProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	peer := startFencePeer(t, ctx, false)
	for _, network := range []string{"tcp4", "udp4", "tcp6", "udp6"} {
		host := "127.0.0.1"
		if strings.HasSuffix(network, "6") {
			host = "::1"
		}
		response := peer.call(t, fencePeerRequest{Op: "listen", ID: network, Network: network, Address: net.JoinHostPort(host, "0")})
		require.Empty(t, response.Error)
		require.Empty(t, peer.call(t, fencePeerRequest{Op: "dial", ID: network, Network: network, Address: response.Address}).Error)
		require.Empty(t, peer.call(t, fencePeerRequest{Op: "exchange", ID: network, Payload: "fixture-check"}).Error)
		require.EqualValues(t, 1, peer.call(t, fencePeerRequest{Op: "count", ID: network}).Count)
	}
	uid := peer.call(t, fencePeerRequest{Op: "uid", UID: os.Geteuid()})
	require.Empty(t, uid.Error)
	require.Equal(t, os.Geteuid(), uid.UID)
}

func kernelCmd(t *testing.T, ctx context.Context, pid int, binary string, args ...string) []byte {
	t.Helper()
	if pid != 0 {
		args = append([]string{"-t", strconv.Itoa(pid), "-n", binary}, args...)
		binary = "nsenter"
	}
	out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	require.NoError(t, err, "%s %v: %s", binary, args, out)
	return out
}

func fenceLinkMAC(t *testing.T, ctx context.Context, pid int, name string) string {
	t.Helper()
	var links []struct {
		Address string `json:"address"`
	}
	require.NoError(t, json.Unmarshal(kernelCmd(t, ctx, pid, "ip", "-j", "link", "show", name), &links))
	require.Len(t, links, 1)
	require.NotEmpty(t, links[0].Address)
	return links[0].Address
}

type fencePeerRequest struct {
	Op, ID, Network, Address, Payload string
	Mark, UID, TimeoutMS              int
}
type fencePeerResponse struct {
	Error, Address, Namespace string
	PID, UID                  int
	Count                     int64
}
type fencePeer struct {
	pid    int
	input  *json.Encoder
	output *json.Decoder
}

func startFencePeer(t *testing.T, ctx context.Context, isolated bool) *fencePeer {
	t.Helper()
	binary, args := os.Args[0], []string{"-test.run=^TestQuarantineFencePeerProcess$"}
	if isolated {
		binary, args = "unshare", append([]string{"--net", os.Args[0]}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "OPENSANDBOX_FENCE_PEER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	peer := &fencePeer{input: json.NewEncoder(stdin), output: json.NewDecoder(stdout)}
	var ready fencePeerResponse
	require.NoError(t, peer.output.Decode(&ready))
	require.Empty(t, ready.Error)
	require.Positive(t, ready.PID)
	peer.pid = ready.PID
	namespace, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)
	if isolated {
		require.NotEqual(t, namespace, ready.Namespace)
	} else {
		require.Equal(t, namespace, ready.Namespace)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		err := cmd.Wait()
		if ctx.Err() == nil {
			require.NoError(t, err)
		}
	})
	return peer
}

func (p *fencePeer) call(t *testing.T, request fencePeerRequest) fencePeerResponse {
	t.Helper()
	require.NoError(t, p.input.Encode(request))
	var response fencePeerResponse
	require.NoError(t, p.output.Decode(&response))
	return response
}

// Re-executed helper uses pipes for control, so quarantine cannot block test
// coordination. It exits directly, keeping Go test progress off the JSON pipe.
func TestQuarantineFencePeerProcess(t *testing.T) {
	if os.Getenv("OPENSANDBOX_FENCE_PEER") != "1" {
		return
	}
	input, output := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		os.Exit(3)
	}
	_ = output.Encode(fencePeerResponse{PID: os.Getpid(), Namespace: namespace, UID: os.Geteuid()})
	connections := map[string]net.Conn{}
	counts := map[string]*atomic.Int64{}
	for {
		var request fencePeerRequest
		if err := input.Decode(&request); err != nil {
			if err == io.EOF {
				os.Exit(0)
			}
			os.Exit(4)
		}
		response := fencePeerResponse{}
		timeout := 2 * time.Second
		if request.TimeoutMS > 0 {
			timeout = time.Duration(request.TimeoutMS) * time.Millisecond
		}
		var opErr error
		switch request.Op {
		case "uid":
			opErr = syscall.Setuid(request.UID)
			response.UID = os.Geteuid()
		case "listen":
			count := &atomic.Int64{}
			counts[request.ID] = count
			if strings.HasPrefix(request.Network, "tcp") {
				var listener net.Listener
				listener, opErr = net.Listen(request.Network, request.Address)
				if opErr == nil {
					response.Address = listener.Addr().String()
					go func() {
						for {
							conn, err := listener.Accept()
							if err != nil {
								return
							}
							go func() {
								defer conn.Close()
								reader := bufio.NewReader(conn)
								for {
									line, err := reader.ReadBytes('\n')
									if err != nil {
										return
									}
									count.Add(1)
									if _, err = conn.Write(line); err != nil {
										return
									}
								}
							}()
						}
					}()
				}
			} else {
				var listener net.PacketConn
				listener, opErr = net.ListenPacket(request.Network, request.Address)
				if opErr == nil {
					response.Address = listener.LocalAddr().String()
					go func() {
						buffer := make([]byte, 4096)
						for {
							n, addr, err := listener.ReadFrom(buffer)
							if err != nil {
								return
							}
							count.Add(1)
							_, _ = listener.WriteTo(buffer[:n], addr)
						}
					}()
				}
			}
		case "dial":
			dialer := net.Dialer{Timeout: timeout}
			if request.Mark != 0 {
				dialer.Control = func(_, _ string, raw syscall.RawConn) error {
					var markErr error
					err := raw.Control(func(fd uintptr) {
						markErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, request.Mark)
					})
					return errors.Join(err, markErr)
				}
			}
			var conn net.Conn
			conn, opErr = dialer.Dial(request.Network, request.Address)
			if opErr == nil {
				connections[request.ID] = conn
			}
		case "exchange":
			conn := connections[request.ID]
			if conn == nil {
				opErr = errors.New("connection missing")
				break
			}
			opErr = conn.SetDeadline(time.Now().Add(timeout))
			if opErr != nil {
				break
			}
			payload := request.Payload + "\n"
			_, opErr = io.WriteString(conn, payload)
			if opErr != nil {
				break
			}
			buffer := make([]byte, len(payload))
			_, opErr = io.ReadFull(conn, buffer)
			if opErr == nil && string(buffer) != payload {
				opErr = fmt.Errorf("incorrect echo: %q", buffer)
			}
		case "count":
			if count := counts[request.ID]; count != nil {
				response.Count = count.Load()
			} else {
				opErr = errors.New("listener missing")
			}
		default:
			opErr = fmt.Errorf("unknown peer operation %q", request.Op)
		}
		if opErr != nil {
			response.Error = opErr.Error()
		}
		if err := output.Encode(response); err != nil {
			os.Exit(5)
		}
	}
}
