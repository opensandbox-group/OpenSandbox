// Copyright 2025 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"fmt"
	"os"
	"path/filepath"
)

func SafeJoin(baseDir, userPath string) (string, error) {
	joinedPath := filepath.Join(baseDir, userPath)

	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base directory absolute path: %w", err)
	}
	absJoinedPath, err := filepath.Abs(joinedPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve joined path absolute path: %w", err)
	}

	if !isSubPath(absBaseDir, absJoinedPath) {
		return "", fmt.Errorf("path traversal detected")
	}

	return absJoinedPath, nil
}

func isSubPath(parent, child string) bool {
	if len(parent) == 0 {
		return false
	}

	parentWithSep := parent
	if !os.IsPathSeparator(parent[len(parent)-1]) {
		parentWithSep = parent + string(filepath.Separator)
	}

	return child == parent || (len(child) > len(parentWithSep) && child[:len(parentWithSep)] == parentWithSep)
}
