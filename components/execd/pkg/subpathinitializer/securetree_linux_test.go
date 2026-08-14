// Copyright 2025 Alibaba Group Holding Ltd.
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

package subpathinitializer

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestParsePlanRejectsUnsafeAndMalformedInput(t *testing.T) {
	tests := []string{
		`[]`,
		`[{"mountPath":"/root","subPaths":[]}]`,
		`[{"mountPath":"/root","subPaths":["a/../b"]}]`,
		`[{"mountPath":"/root","subPaths":["a//b"]}]`,
		`[{"mountPath":"/root","subPaths":["a"]},{"mountPath":"/root","subPaths":["b"]}]`,
		`[{"mountPath":"/root","subPaths":["a"],"unknown":true}]`,
		`[{"mountPath":"/root","subPaths":["a"]}] garbage`,
	}
	for _, raw := range tests {
		if _, err := ParsePlan(raw); err == nil {
			t.Fatalf("ParsePlan(%q) succeeded", raw)
		}
	}
}

func TestApplyCreatesDirectoriesWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"jobs/a", "jobs/b"}}}, nil); err != nil {
		t.Fatalf("Apply(): %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "jobs", "a")); err != nil || !info.IsDir() {
		t.Fatalf("created directory: info=%v err=%v", info, err)
	}

	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"link/escape"}}}, nil); err == nil {
		t.Fatal("Apply followed a symlink")
	}
}

func TestApplyWithoutFSGroupUsesDefaultDirectoryMode(t *testing.T) {
	root := t.TempDir()
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"jobs/a"}}}, nil); err != nil {
		t.Fatalf("Apply(): %v", err)
	}

	for _, path := range []string{"jobs", "jobs/a"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if mode := info.Mode().Perm(); mode != 0755 {
			t.Errorf("mode for %q = %#o, want 0755", path, mode)
		}
	}
}

func TestApplyFSGroupChangesOnlyCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0711); err != nil {
		t.Fatal(err)
	}
	before := stat(t, existing)

	fsGroup := os.Getgid()
	if fsGroup == 0 {
		fsGroup = 1
	}
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"existing/new/nested"}}}, &fsGroup); err != nil {
		t.Fatalf("Apply(): %v", err)
	}

	after := stat(t, existing)
	if mode := after.Mode & 07777; mode != before.Mode&07777 {
		t.Errorf("existing directory mode = %#o, want %#o", mode, before.Mode&07777)
	}
	if after.Gid != before.Gid {
		t.Errorf("existing directory gid = %d, want %d", after.Gid, before.Gid)
	}
	for _, path := range []string{"existing/new", "existing/new/nested"} {
		info := stat(t, filepath.Join(root, path))
		if mode := info.Mode & 07777; mode != 02770 {
			t.Errorf("mode for %q = %#o, want 02770", path, mode)
		}
		if info.Gid != uint32(fsGroup) {
			t.Errorf("gid for %q = %d, want %d", path, info.Gid, fsGroup)
		}
	}
}

func TestApplyRejectsInvalidFSGroup(t *testing.T) {
	root := t.TempDir()
	zero := 0
	negative := -1
	invalidGroups := []*int{&zero, &negative}
	if strconv.IntSize > 32 {
		overflow := int(int64(2147483647) + 1)
		invalidGroups = append(invalidGroups, &overflow)
	}

	for _, fsGroup := range invalidGroups {
		if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"jobs"}}}, fsGroup); err == nil {
			t.Errorf("Apply(fsGroup=%d) succeeded", *fsGroup)
		}
	}
}

func stat(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	result, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q returned %T, want *syscall.Stat_t", path, info.Sys())
	}
	return result
}
