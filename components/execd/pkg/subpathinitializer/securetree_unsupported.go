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

//go:build !linux

// Package subpathinitializer creates planned directories without following links.
package subpathinitializer

import "fmt"

// PlanEntry names one trusted PVC root mount and the relative paths to create beneath it.
type PlanEntry struct {
	MountPath string   `json:"mountPath"`
	SubPaths  []string `json:"subPaths"`
}

// ParsePlan fails closed because secure fd-relative directory creation is Linux-only.
func ParsePlan(string) ([]PlanEntry, error) {
	return nil, fmt.Errorf("subpath initialization is supported only on linux")
}

// Apply fails closed because secure fd-relative directory creation is Linux-only.
func Apply([]PlanEntry) error {
	return fmt.Errorf("subpath initialization is supported only on linux")
}
