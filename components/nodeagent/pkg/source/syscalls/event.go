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
	"fmt"
	"strings"
)

func decodeKernelEvent(raw []byte) (kernelEvent, error) {
	if len(raw) != 56 {
		return kernelEvent{}, fmt.Errorf("unexpected syscall event size %d", len(raw))
	}
	return kernelEvent{
		MonotonicNS: binary.LittleEndian.Uint64(raw[0:8]),
		CgroupID:    binary.LittleEndian.Uint64(raw[8:16]),
		Handle:      binary.LittleEndian.Uint64(raw[16:24]),
		HostPID:     binary.LittleEndian.Uint32(raw[24:28]),
		HostTID:     binary.LittleEndian.Uint32(raw[28:32]),
		SyscallNR:   int64(binary.LittleEndian.Uint64(raw[32:40])),
		Comm:        strings.TrimRight(string(raw[40:56]), "\x00"),
	}, nil
}
