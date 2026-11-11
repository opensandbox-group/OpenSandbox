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

package cgroupfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEntries(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantV2    string
		wantV1CPU string
		wantV1Mem string
	}{
		{
			name:   "pure cgroup v2",
			data:   "0::/system.slice/docker/abc\n",
			wantV2: "/system.slice/docker/abc",
		},
		{
			name:      "pure cgroup v1",
			data:      "10:cpuset:/\n12:memory:/sandbox\n3:cpu,cpuacct:/sandbox\n",
			wantV1CPU: "/sandbox",
			wantV1Mem: "/sandbox",
		},
		{
			name:      "hybrid host refuses the degenerate v2 entry",
			data:      "10:cpuset:/a\n0::/\n12:memory:/x\n",
			wantV1Mem: "/x",
		},
		{
			name: "empty",
			data: "",
		},
		{
			name:   "malformed lines skipped",
			data:   "garbage\n0::\n0::/ok\n",
			wantV2: "/ok",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := ParseEntries([]byte(tt.data))
			if got := V2RelPath(entries); got != tt.wantV2 {
				t.Fatalf("V2RelPath = %q, want %q", got, tt.wantV2)
			}
			if got := V1RelPath(entries, "cpu", "cpuacct"); got != tt.wantV1CPU {
				t.Fatalf("V1RelPath(cpu) = %q, want %q", got, tt.wantV1CPU)
			}
			if got := V1RelPath(entries, "memory"); got != tt.wantV1Mem {
				t.Fatalf("V1RelPath(memory) = %q, want %q", got, tt.wantV1Mem)
			}
		})
	}
}

func TestOptionsHave(t *testing.T) {
	if !OptionsHave([]string{"rw,nosuid,nodev,memory"}, "memory") {
		t.Fatal("comma-separated single field should match")
	}
	if !OptionsHave([]string{"rw", "relatime", "memory"}, "memory") {
		t.Fatal("split fields should match")
	}
	if !OptionsHave([]string{"rw,cpu,cpuacct"}, "cpu") {
		t.Fatal("combined controller list should match")
	}
	if OptionsHave([]string{"rw,nosuid,nodev"}, "memory") {
		t.Fatal("absent controller should not match")
	}
	if OptionsHave(nil, "memory") {
		t.Fatal("no options should not match")
	}
	// Prefix collisions must not match: "memory-full" is not "memory".
	if OptionsHave([]string{"memory-full"}, "memory") {
		t.Fatal("prefix collision should not match")
	}
}

func TestScanMounts(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "mounts")
	content := `sysfs /sys sysfs rw,nosuid,nodev,noexec 0 0
cgroup /sys/fs/cgroup/memory cgroup rw,nosuid,nodev,memory 0 0
cgroup /sys/fs/cgroup/cpu,cpuacct cgroup rw,nosuid,nodev,cpu,cpuacct 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw 0 0
`
	if err := os.WriteFile(fixture, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := mountsFile
	mountsFile = fixture
	t.Cleanup(func() { mountsFile = orig })

	mount, err := ScanMounts(func(_, fstype string, options []string) bool {
		return fstype == "cgroup" && OptionsHave(options, "memory")
	})
	if err != nil || mount != "/sys/fs/cgroup/memory" {
		t.Fatalf("memory mount = (%q, %v), want /sys/fs/cgroup/memory", mount, err)
	}
	mount, err = ScanMounts(func(_, fstype string, options []string) bool {
		return fstype == "cgroup" && OptionsHave(options, "cpuacct")
	})
	if err != nil || mount != "/sys/fs/cgroup/cpu,cpuacct" {
		t.Fatalf("cpuacct mount = (%q, %v), want /sys/fs/cgroup/cpu,cpuacct", mount, err)
	}
	mount, err = ScanMounts(func(_, fstype string, _ []string) bool { return fstype == "cgroup2" })
	if err != nil || mount != "/sys/fs/cgroup" {
		t.Fatalf("cgroup2 mount = (%q, %v), want /sys/fs/cgroup", mount, err)
	}
	if _, err := ScanMounts(func(_, fstype string, _ []string) bool { return fstype == "devtmpfs" }); err == nil {
		t.Fatal("unmatched fstype should fail")
	}
}

func TestDir(t *testing.T) {
	// The second line mirrors Docker on cgroup v1 without a cgroup
	// namespace: the memory mount exposes the container's own subtree.
	mountinfo := `25 1 0:23 / /sys/fs/cgroup rw,nosuid - tmpfs tmpfs ro,mode=755
694 683 0:32 /docker/abc /sys/fs/cgroup/memory ro,nosuid,nodev,noexec,relatime master:15 - cgroup cgroup rw,memory
30 25 0:26 / /sys/fs/cgroup/pids rw,nosuid shared:9 - cgroup cgroup rw,pids
31 25 0:27 / /sys/fs/cgroup/cpu,cpuacct rw,nosuid shared:10 - cgroup cgroup rw,cpu,cpuacct
40 25 0:27 /kubepods/pod1 /sys/fs/cgroup/cpu,cpuacct rw,nosuid shared:10 - cgroup cgroup rw,cpu,cpuacct
50 25 0:28 /docker/abc /mnt/cg\040memory rw - cgroup cgroup rw,memory
`
	tests := []struct {
		name       string
		mountinfo  string
		mountPoint string
		cgroupPath string
		want       string
	}{
		{
			name:       "mount rooted at the process cgroup",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/memory",
			cgroupPath: "/docker/abc",
			want:       "/sys/fs/cgroup/memory",
		},
		{
			name:       "process cgroup nested below the mount root",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/memory",
			cgroupPath: "/docker/abc/session",
			want:       "/sys/fs/cgroup/memory/session",
		},
		{
			name:       "hierarchy root mount joins the full path",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/pids",
			cgroupPath: "/docker/abc",
			want:       "/sys/fs/cgroup/pids/docker/abc",
		},
		{
			name:       "topmost stacked mount wins",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/cpu,cpuacct",
			cgroupPath: "/kubepods/pod1",
			want:       "/sys/fs/cgroup/cpu,cpuacct",
		},
		{
			name:       "escaped mount point",
			mountinfo:  mountinfo,
			mountPoint: "/mnt/cg memory",
			cgroupPath: "/docker/abc",
			want:       "/mnt/cg memory",
		},
		{
			name:       "path outside the mount root joins unchanged",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/memory",
			cgroupPath: "/docker/abcdef",
			want:       "/sys/fs/cgroup/memory/docker/abcdef",
		},
		{
			name:       "mount absent from mountinfo joins unchanged",
			mountinfo:  mountinfo,
			mountPoint: "/sys/fs/cgroup/blkio",
			cgroupPath: "/docker/abc",
			want:       "/sys/fs/cgroup/blkio/docker/abc",
		},
		{
			name:       "unreadable mountinfo joins unchanged",
			mountPoint: "/sys/fs/cgroup/memory",
			cgroupPath: "/docker/abc",
			want:       "/sys/fs/cgroup/memory/docker/abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Dir([]byte(tt.mountinfo), tt.mountPoint, tt.cgroupPath); got != tt.want {
				t.Fatalf("Dir = %q, want %q", got, tt.want)
			}
		})
	}
}
