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

package syscalls

import (
	"encoding/binary"
	"testing"
)

func TestDecodeKernelEvent(t *testing.T) {
	raw := make([]byte, 56)
	binary.LittleEndian.PutUint64(raw[0:8], 100)
	binary.LittleEndian.PutUint64(raw[8:16], 200)
	binary.LittleEndian.PutUint64(raw[16:24], 300)
	binary.LittleEndian.PutUint32(raw[24:28], 400)
	binary.LittleEndian.PutUint32(raw[28:32], 401)
	binary.LittleEndian.PutUint64(raw[32:40], 63)
	copy(raw[40:56], "cat")
	event, err := decodeKernelEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if event.MonotonicNS != 100 || event.CgroupID != 200 || event.Handle != 300 || event.HostPID != 400 || event.HostTID != 401 || event.SyscallNR != 63 || event.Comm != "cat" {
		t.Fatalf("event=%+v", event)
	}
}
