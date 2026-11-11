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

package controller

import (
	"os"
	"strconv"
)

// fileModeFromAPI converts an API mode such as 4755 into a Go file mode.
// The integer's decimal digits are octal. Setuid, setgid, and sticky live in
// Go's ModeSetuid, ModeSetgid, and ModeSticky bits, not in the low 12 bits,
// so passing the parsed integer straight to os.Chmod drops them.
func fileModeFromAPI(mode int) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(strconv.Itoa(mode), 8, 32)
	if err != nil {
		return 0, err
	}
	fileMode := os.FileMode(parsed & 0o777)
	if parsed&0o4000 != 0 {
		fileMode |= os.ModeSetuid
	}
	if parsed&0o2000 != 0 {
		fileMode |= os.ModeSetgid
	}
	if parsed&0o1000 != 0 {
		fileMode |= os.ModeSticky
	}
	return fileMode, nil
}

// apiModeFromFileMode reports a Go mode as the octal integer the API uses,
// including setuid, setgid, and sticky. 0755 is 755 and 04755 is 4755.
func apiModeFromFileMode(mode os.FileMode) int {
	unix := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		unix |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		unix |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		unix |= 0o1000
	}
	reported, _ := strconv.Atoi(strconv.FormatUint(uint64(unix), 8))
	return reported
}
