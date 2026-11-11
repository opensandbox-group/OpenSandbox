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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

func TestContainerCgroupNamesIncludeSystemdAndCgroupfsForms(t *testing.T) {
	containerID := "abcdef"
	for _, want := range []string{"crio-abcdef", "crio-abcdef.scope"} {
		if !slices.Contains(containerCgroupNames("cri-o", containerID), want) {
			t.Fatalf("CRI-O cgroup names do not contain %q", want)
		}
	}
	for _, want := range []string{"docker-abcdef", "docker-abcdef.scope"} {
		if !slices.Contains(containerCgroupNames("docker", containerID), want) {
			t.Fatalf("Docker cgroup names do not contain %q", want)
		}
	}
}

func TestResolveCgroupsRequiresFullContainerIDAndPodAncestor(t *testing.T) {
	root := t.TempDir()
	podUID := "12345678-abcd-4321-abcd-1234567890ab"
	containerID := "0123456789abcdef"
	valid := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod12345678_abcd_4321_abcd_1234567890ab.slice", "cri-containerd-"+containerID+".scope")
	wrongPod := filepath.Join(root, "kubepods", "podother", containerID)
	prefixOnly := filepath.Join(root, "kubepods", "pod"+podUID, containerID[:12])
	for _, path := range []string{valid, wrongPod, prefixOnly} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resource := store.Resource{Resource: api.Resource{PodUID: podUID}, ContainerRuntime: "containerd", ContainerID: containerID}
	resolved, err := resolveCgroupsWithInode(root, []store.Resource{resource}, func(path string) (uint64, error) {
		if path != valid {
			t.Fatalf("resolved unexpected path %q", path)
		}
		return 42, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved[podUID]; got.id != 42 || got.path != valid {
		t.Fatalf("resolved=%+v", got)
	}
}

func TestResolveCgroupsRejectsAmbiguousCandidates(t *testing.T) {
	root := t.TempDir()
	podUID := "u1"
	containerID := "abcdef"
	for _, qos := range []string{"burstable", "besteffort"} {
		path := filepath.Join(root, qos, "pod"+podUID, containerID)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resource := store.Resource{Resource: api.Resource{PodUID: podUID}, ContainerID: containerID}
	resolved, err := resolveCgroupsWithInode(root, []store.Resource{resource}, func(string) (uint64, error) { return 1, nil })
	if err == nil {
		t.Fatal("ambiguous cgroup candidates were accepted")
	}
	if len(resolved) != 0 {
		t.Fatalf("ambiguous cgroup was resolved: %+v", resolved)
	}
}

func TestResolveCgroupsKeepsOtherResultsAfterAmbiguousCandidate(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "a", "podu1", "container-1"),
		filepath.Join(root, "b", "podu1", "container-1"),
		filepath.Join(root, "c", "podu2", "container-2"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resources := []store.Resource{
		{Resource: api.Resource{PodUID: "u1"}, ContainerID: "container-1"},
		{Resource: api.Resource{PodUID: "u2"}, ContainerID: "container-2"},
	}
	resolved, err := resolveCgroupsWithInode(root, resources, func(path string) (uint64, error) {
		if filepath.Base(path) == "container-2" {
			return 2, nil
		}
		return 1, nil
	})
	if err == nil {
		t.Fatal("ambiguous cgroup was not reported")
	}
	if _, found := resolved["u1"]; found || resolved["u2"].id != 2 {
		t.Fatalf("partial resolution=%+v", resolved)
	}
}

func TestResolveCgroupsIgnoresVanishedCandidate(t *testing.T) {
	root := t.TempDir()
	podUID := "u1"
	containerID := "abcdef"
	path := filepath.Join(root, "pod"+podUID, containerID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	resource := store.Resource{Resource: api.Resource{PodUID: podUID}, ContainerID: containerID}
	resolved, err := resolveCgroupsWithInode(root, []store.Resource{resource}, func(string) (uint64, error) {
		return 0, fs.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved vanished candidate: %+v", resolved)
	}
}
