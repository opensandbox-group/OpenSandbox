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

package isolation

import (
	"fmt"
	"os"
)

// DefaultSeccompPath is where execd looks for a seccomp BPF profile.
const DefaultSeccompPath = "/etc/execd/seccomp.bpf"

// LoadSeccomp opens a seccomp BPF file and returns its path for passing
// to bwrap --seccomp. Returns empty string if no profile is found (seccomp
// is then skipped).
func LoadSeccomp(path string) (string, error) {
	if path == "" {
		path = DefaultSeccompPath
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No profile is fine — bwrap runs without seccomp.
			return "", nil
		}
		return "", fmt.Errorf("seccomp: open %s: %w", path, err)
	}
	defer f.Close()

	// Verify the file is readable and non-empty.
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("seccomp: stat %s: %w", path, err)
	}
	if st.Size() == 0 {
		return "", nil
	}

	return path, nil
}
