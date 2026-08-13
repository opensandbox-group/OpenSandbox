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
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"jobs/a", "jobs/b"}}}); err != nil {
		t.Fatalf("Apply(): %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "jobs", "a")); err != nil || !info.IsDir() {
		t.Fatalf("created directory: info=%v err=%v", info, err)
	}

	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Apply([]PlanEntry{{MountPath: root, SubPaths: []string{"link/escape"}}}); err == nil {
		t.Fatal("Apply followed a symlink")
	}
}
