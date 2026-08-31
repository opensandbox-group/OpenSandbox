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

package resourcecheck

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const procCgroupPath = "/proc/self/cgroup"

func TestCgroupMemoryLimitWithReadFile(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		readErrs    map[string]error
		wantLimit   uint64
		wantLimited bool
		wantErrPath string
	}{
		{
			name: "finite v2 limit",
			files: map[string]string{
				procCgroupPath: "0::/kubepods/pod-a\n",
				"/sys/fs/cgroup/kubepods/pod-a/memory.max": "536870912\n",
			},
			wantLimit:   536870912,
			wantLimited: true,
		},
		{
			name: "v2 max is unlimited",
			files: map[string]string{
				procCgroupPath: "0::/kubepods/pod-a\n",
				"/sys/fs/cgroup/kubepods/pod-a/memory.max": "max\n",
			},
		},
		{
			name: "v1 sentinel is unlimited",
			files: map[string]string{
				procCgroupPath: "5:memory:/kubepods/pod-a\n",
				"/sys/fs/cgroup/memory/kubepods/pod-a/memory.limit_in_bytes": "1152921504606846976\n",
			},
		},
		{
			name: "missing candidates are unlimited",
			files: map[string]string{
				procCgroupPath: "0::/kubepods/pod-a\n",
			},
		},
		{
			name: "invalid exact candidate fails despite valid fallback",
			files: map[string]string{
				procCgroupPath: "0::/kubepods/pod-a\n",
				"/sys/fs/cgroup/kubepods/pod-a/memory.max": "not-a-number\n",
				"/sys/fs/cgroup/memory.max":                "268435456\n",
			},
			wantErrPath: "/sys/fs/cgroup/kubepods/pod-a/memory.max",
		},
		{
			name: "unreadable exact candidate fails despite valid fallback",
			files: map[string]string{
				procCgroupPath:              "0::/kubepods/pod-a\n",
				"/sys/fs/cgroup/memory.max": "max\n",
			},
			readErrs: map[string]error{
				"/sys/fs/cgroup/kubepods/pod-a/memory.max": errors.New("permission denied"),
			},
			wantErrPath: "/sys/fs/cgroup/kubepods/pod-a/memory.max",
		},
		{
			name: "proc cgroup read failure",
			readErrs: map[string]error{
				procCgroupPath: errors.New("I/O error"),
			},
			wantErrPath: procCgroupPath,
		},
		{
			name: "invalid sole candidate",
			files: map[string]string{
				procCgroupPath: "0::/kubepods/pod-a\n",
				"/sys/fs/cgroup/kubepods/pod-a/memory.max": "not-a-number\n",
			},
			wantErrPath: "/sys/fs/cgroup/kubepods/pod-a/memory.max",
		},
		{
			name: "unreadable sole candidate",
			files: map[string]string{
				procCgroupPath: "5:memory:/kubepods/pod-a\n",
			},
			readErrs: map[string]error{
				"/sys/fs/cgroup/memory/kubepods/pod-a/memory.limit_in_bytes": errors.New("permission denied"),
			},
			wantErrPath: "/sys/fs/cgroup/memory/kubepods/pod-a/memory.limit_in_bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readFile := func(path string) ([]byte, error) {
				if err, ok := tt.readErrs[path]; ok {
					return nil, err
				}
				if contents, ok := tt.files[path]; ok {
					return []byte(contents), nil
				}
				return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
			}

			limit, limited, err := cgroupMemoryLimitWithReadFile(readFile)
			if tt.wantErrPath != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPath) {
					t.Fatalf("cgroupMemoryLimitWithReadFile() error = %v, want path %q", err, tt.wantErrPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("cgroupMemoryLimitWithReadFile() error = %v", err)
			}
			if limit != tt.wantLimit || limited != tt.wantLimited {
				t.Fatalf("cgroupMemoryLimitWithReadFile() = (%d, %t), want (%d, %t)", limit, limited, tt.wantLimit, tt.wantLimited)
			}
		})
	}
}
