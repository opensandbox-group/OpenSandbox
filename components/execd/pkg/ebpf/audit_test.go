//go:build ebpf

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

package ebpf

import (
	"encoding/binary"
	"testing"
)

func TestDecodeExecEvent(t *testing.T) {
	raw := make([]byte, sizeEventExec)
	binary.LittleEndian.PutUint32(raw[0:4], 42)
	binary.LittleEndian.PutUint32(raw[4:8], 1)
	copy(raw[8:24], "python3")
	copy(raw[24:88], "/usr/bin/python3")
	ev, ok := decodeEvent(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if ev.Event != "exec" || ev.PID != 42 || ev.PPID != 1 {
		t.Fatalf("exec envelope = %+v", ev)
	}
	if ev.Comm != "python3" || ev.Filename != "/usr/bin/python3" {
		t.Fatalf("exec fields = %q/%q", ev.Comm, ev.Filename)
	}
	if ev.TS == "" || ev.SandboxID != "" {
		t.Fatalf("envelope ts/sandbox_id = %q/%q", ev.TS, ev.SandboxID)
	}
}

func TestDecodeConnectEvent(t *testing.T) {
	raw := make([]byte, sizeEventConnect)
	binary.LittleEndian.PutUint32(raw[0:4], 7)
	copy(raw[4:20], "curl")
	// IPv4-mapped ::ffff:5d:b8:d8:22 (93.184.216.34)
	copy(raw[20:24], []byte{0, 0, 0, 0})
	copy(raw[24:28], []byte{0, 0, 0, 0})
	copy(raw[28:32], []byte{0, 0, 0xff, 0xff})
	copy(raw[32:36], []byte{93, 184, 216, 34})
	binary.BigEndian.PutUint16(raw[36:38], 443)

	ev, ok := decodeEvent(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if ev.Event != "connect" || ev.PID != 7 || ev.Comm != "curl" {
		t.Fatalf("connect envelope = %+v", ev)
	}
	if ev.DstIP != "93.184.216.34" || ev.DstPort != 443 || ev.Proto != "tcp" {
		t.Fatalf("connect dst = %s:%d/%s", ev.DstIP, ev.DstPort, ev.Proto)
	}
}

func TestDecodeConnectEventIPv6(t *testing.T) {
	raw := make([]byte, sizeEventConnect)
	binary.LittleEndian.PutUint32(raw[0:4], 9)
	copy(raw[4:20], "curl")
	// 2606:4700::6810:84e5 stored big-endian
	ip := []byte{0x26, 0x06, 0x47, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0x68, 0x10, 0x84, 0xe5}
	copy(raw[20:36], ip)
	binary.BigEndian.PutUint16(raw[36:38], 80)

	ev, ok := decodeEvent(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if ev.DstIP != "2606:4700::6810:84e5" || ev.DstPort != 80 {
		t.Fatalf("ipv6 dst = %s:%d", ev.DstIP, ev.DstPort)
	}
}

func TestDecodePrivilegeEvent(t *testing.T) {
	raw := make([]byte, sizeEventPrivilege)
	binary.LittleEndian.PutUint32(raw[0:4], 57)
	copy(raw[4:20], "sudo")
	binary.LittleEndian.PutUint32(raw[20:24], 1000)
	binary.LittleEndian.PutUint32(raw[24:28], 0)
	binary.LittleEndian.PutUint32(raw[28:32], 1000)
	binary.LittleEndian.PutUint32(raw[32:36], 0)
	binary.LittleEndian.PutUint64(raw[36:44], 1<<7)

	ev, ok := decodeEvent(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if ev.Event != "privilege" || ev.PID != 57 || ev.Comm != "sudo" {
		t.Fatalf("privilege envelope = %+v", ev)
	}
	if ev.OldUID != 1000 || ev.NewUID != 0 {
		t.Fatalf("uid change = %d -> %d", ev.OldUID, ev.NewUID)
	}
	if len(ev.CapAdded) != 1 || ev.CapAdded[0] != "CAP_SETUID" {
		t.Fatalf("cap_added = %v", ev.CapAdded)
	}
}

func TestDecodeUnknownSize(t *testing.T) {
	if _, ok := decodeEvent(make([]byte, 3)); ok {
		t.Fatal("unknown size decoded")
	}
}
