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

// Package subpathinitializer creates planned directories without following links.
package subpathinitializer

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// PlanEntry names one trusted PVC root mount and the relative paths to create beneath it.
type PlanEntry struct {
	MountPath string   `json:"mountPath"`
	SubPaths  []string `json:"subPaths"`
}

// ParsePlan accepts exactly the renderer's JSON plan shape.
func ParsePlan(raw string) ([]PlanEntry, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var plan []PlanEntry
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode plan: trailing data")
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("plan must contain at least one entry")
	}

	mounts := make(map[string]struct{}, len(plan))
	for index, entry := range plan {
		if err := validateMountPath(entry.MountPath); err != nil {
			return nil, fmt.Errorf("plan entry %d: %w", index, err)
		}
		if _, exists := mounts[entry.MountPath]; exists {
			return nil, fmt.Errorf("plan entry %d: duplicate mountPath", index)
		}
		mounts[entry.MountPath] = struct{}{}
		if len(entry.SubPaths) == 0 {
			return nil, fmt.Errorf("plan entry %d: subPaths must not be empty", index)
		}
		paths := make(map[string]struct{}, len(entry.SubPaths))
		for _, subPath := range entry.SubPaths {
			if err := validateSubPath(subPath); err != nil {
				return nil, fmt.Errorf("plan entry %d: %w", index, err)
			}
			if _, exists := paths[subPath]; exists {
				return nil, fmt.Errorf("plan entry %d: duplicate subPath", index)
			}
			paths[subPath] = struct{}{}
		}
	}
	return plan, nil
}

// Apply creates every planned directory relative to an O_NOFOLLOW root descriptor.
func Apply(plan []PlanEntry, fsGroup *int) error {
	if err := validateFSGroup(fsGroup); err != nil {
		return err
	}
	if len(plan) == 0 {
		return fmt.Errorf("plan must contain at least one entry")
	}
	for _, entry := range plan {
		if err := validateMountPath(entry.MountPath); err != nil {
			return err
		}
		if len(entry.SubPaths) == 0 {
			return fmt.Errorf("subPaths must not be empty")
		}
		rootFD, err := unix.Open(entry.MountPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open root %q: %w", entry.MountPath, err)
		}
		for _, subPath := range entry.SubPaths {
			if err := validateSubPath(subPath); err != nil {
				unix.Close(rootFD)
				return err
			}
			if err := mkdirAt(rootFD, subPath, fsGroup); err != nil {
				unix.Close(rootFD)
				return fmt.Errorf("create %q beneath %q: %w", subPath, entry.MountPath, err)
			}
		}
		if err := unix.Close(rootFD); err != nil {
			return fmt.Errorf("close root %q: %w", entry.MountPath, err)
		}
	}
	return nil
}

func mkdirAt(rootFD int, subPath string, fsGroup *int) error {
	currentFD := rootFD
	for _, segment := range strings.Split(subPath, "/") {
		created := false
		if err := unix.Mkdirat(currentFD, segment, 0755); err == nil {
			created = true
		} else if err != unix.EEXIST {
			if currentFD != rootFD {
				unix.Close(currentFD)
			}
			return err
		}
		nextFD, err := unix.Openat(currentFD, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if currentFD != rootFD {
			unix.Close(currentFD)
		}
		if err != nil {
			return err
		}
		if created && fsGroup != nil {
			if err := unix.Fchown(nextFD, -1, *fsGroup); err != nil {
				unix.Close(nextFD)
				return err
			}
			if err := unix.Fchmod(nextFD, 02770); err != nil {
				unix.Close(nextFD)
				return err
			}
		}
		currentFD = nextFD
	}
	if currentFD != rootFD {
		return unix.Close(currentFD)
	}
	return nil
}

func validateFSGroup(fsGroup *int) error {
	if fsGroup != nil && (*fsGroup < 1 || *fsGroup > 2147483647) {
		return fmt.Errorf("fsGroup must be between 1 and 2147483647")
	}
	return nil
}

func validateMountPath(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("mountPath must be a normalized absolute path")
	}
	return nil
}

func validateSubPath(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return fmt.Errorf("subPath must be a non-empty normalized relative path")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("subPath must not contain empty, '.' or '..' segments")
		}
	}
	return nil
}
