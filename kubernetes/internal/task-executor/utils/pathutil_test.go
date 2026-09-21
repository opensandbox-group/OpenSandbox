// Copyright 2025 The OpenSandbox Authors
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

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "safejoin-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tests := []struct {
		name     string
		baseDir  string
		userPath string
		wantErr  bool
	}{
		{
			name:     "valid path",
			baseDir:  tempDir,
			userPath: "foo",
			wantErr:  false,
		},
		{
			name:     "valid nested path",
			baseDir:  tempDir,
			userPath: "foo/bar",
			wantErr:  false,
		},
		{
			name:     "path traversal attempt",
			baseDir:  tempDir,
			userPath: "../foo",
			wantErr:  true,
		},
		{
			name:     "path traversal to root (treated as relative)",
			baseDir:  tempDir,
			userPath: "/etc/passwd",
			wantErr:  false,
		},
		{
			name:     "complex traversal",
			baseDir:  tempDir,
			userPath: "foo/../../bar",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafeJoin(tt.baseDir, tt.userPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("SafeJoin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				expected := filepath.Join(tt.baseDir, tt.userPath)
				absExpected, _ := filepath.Abs(expected)
				if got != absExpected {
					t.Errorf("SafeJoin() = %v, want %v", got, absExpected)
				}
			}
		})
	}
}
