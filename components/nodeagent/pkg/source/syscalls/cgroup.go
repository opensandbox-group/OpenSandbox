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
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/nodeagent/pkg/store"
)

type resolvedCgroup struct {
	id   uint64
	path string
}

func resolveCgroups(root string, resources []store.Resource) (map[string]resolvedCgroup, error) {
	return resolveCgroupsWithInode(root, resources, directoryInode)
}

func resolveCgroupsWithInode(root string, resources []store.Resource, inode func(string) (uint64, error)) (map[string]resolvedCgroup, error) {
	byLeaf := make(map[string][]store.Resource)
	for _, resource := range resources {
		if resource.Terminated || resource.ContainerID == "" {
			continue
		}
		for _, leaf := range containerCgroupNames(resource.ContainerRuntime, resource.ContainerID) {
			byLeaf[leaf] = append(byLeaf[leaf], resource)
		}
	}
	if len(byLeaf) == 0 {
		return map[string]resolvedCgroup{}, nil
	}
	resolved := make(map[string]resolvedCgroup)
	invalid := make(map[string]bool)
	var resolutionErrors []error
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		candidates := byLeaf[entry.Name()]
		if len(candidates) == 0 {
			return nil
		}
		for _, resource := range candidates {
			if invalid[resource.PodUID] {
				continue
			}
			if resource.PodUID != "" && !pathContainsPodUID(path, resource.PodUID) {
				continue
			}
			if previous, exists := resolved[resource.PodUID]; exists && previous.path != path {
				delete(resolved, resource.PodUID)
				invalid[resource.PodUID] = true
				resolutionErrors = append(resolutionErrors, fmt.Errorf("container cgroup for Pod %s is ambiguous: %s and %s", resource.PodUID, previous.path, path))
				continue
			}
			id, err := inode(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				invalid[resource.PodUID] = true
				resolutionErrors = append(resolutionErrors, fmt.Errorf("read container cgroup %s: %w", path, err))
				continue
			}
			resolved[resource.PodUID] = resolvedCgroup{id: id, path: path}
		}
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	return resolved, errors.Join(resolutionErrors...)
}

func containerCgroupNames(runtimeName, containerID string) []string {
	if containerID == "" {
		return nil
	}
	names := []string{containerID}
	prefixes := []string{"cri-containerd-", "crio-", "docker-"}
	if runtimeName != "" {
		switch runtimeName {
		case "containerd":
			prefixes = []string{"cri-containerd-"}
		case "cri-o", "crio":
			prefixes = []string{"crio-"}
		case "docker":
			prefixes = []string{"docker-"}
		}
	}
	for _, prefix := range prefixes {
		names = append(names, prefix+containerID, prefix+containerID+".scope")
	}
	return names
}

func pathContainsPodUID(path, podUID string) bool {
	normalizedUID := strings.ReplaceAll(podUID, "-", "_")
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == "pod"+podUID || strings.HasSuffix(segment, "-pod"+normalizedUID+".slice") {
			return true
		}
	}
	return false
}
