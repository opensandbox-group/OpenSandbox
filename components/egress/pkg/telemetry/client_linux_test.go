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

package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// skipIfUnprivileged: setting SO_MARK needs CAP_NET_ADMIN, so unprivileged CI cannot
// exercise the happy path. Anything other than EPERM is a real failure.
func skipIfUnprivileged(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, unix.EPERM) {
		t.Skip("SO_MARK requires CAP_NET_ADMIN; skipping in unprivileged environment")
	}
}

// readSoMark reads SO_MARK back off a live connection.
func readSoMark(t *testing.T, conn net.Conn) int {
	t.Helper()
	sc, ok := conn.(syscall.Conn)
	if !ok {
		t.Fatalf("conn %T does not expose SyscallConn", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var mark int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		mark, getErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
	}); err != nil {
		t.Fatalf("raw.Control: %v", err)
	}
	if getErr != nil {
		t.Fatalf("getsockopt SO_MARK: %v", getErr)
	}
	return mark
}

// The exporter client must carry SO_MARK: the sidecar shares a netns with the sandbox
// and is subject to the policy it installs, so unmarked exports are dropped.
func TestExporterHTTPClientSetsSoMark(t *testing.T) {
	client := exporterHTTPClient()
	if client == nil {
		t.Fatal("exporterHTTPClient() = nil on linux, want a client")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	if err != nil {
		skipIfUnprivileged(t, err)
		t.Fatalf("request through marked client: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// Dialer.Control does not cover name resolution, so the resolver needs its own marked
// dialer: with a hostname endpoint the lookup would otherwise be redirected to the
// policy proxy and answered NXDOMAIN before any connection was attempted.
func TestExporterDialerMarksDNSLookups(t *testing.T) {
	resolver := exporterDialer().Resolver
	if resolver == nil {
		t.Fatal("exporterDialer().Resolver = nil, DNS lookups would go out unmarked")
	}
	if !resolver.PreferGo {
		t.Error("PreferGo = false; the cgo resolver ignores Dial and would not mark lookups")
	}
	if resolver.Dial == nil {
		t.Fatal("Resolver.Dial = nil, lookups would use an unmarked dialer")
	}

	// Stand in for the nameserver: the resolver only needs a dialable UDP address.
	ns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer ns.Close()

	conn, err := resolver.Dial(context.Background(), "udp", ns.LocalAddr().String())
	if err != nil {
		skipIfUnprivileged(t, err)
		t.Fatalf("resolver dial: %v", err)
	}
	defer conn.Close()

	if mark := readSoMark(t, conn); mark != constants.MarkValue {
		t.Errorf("resolver socket SO_MARK = %#x, want %#x", mark, constants.MarkValue)
	}
}

// The connection to the collector itself must be marked too, not just the lookup.
func TestExporterDialerMarksConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()

	conn, err := exporterDialer().DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		skipIfUnprivileged(t, err)
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if mark := readSoMark(t, conn); mark != constants.MarkValue {
		t.Errorf("connection SO_MARK = %#x, want %#x", mark, constants.MarkValue)
	}
}
