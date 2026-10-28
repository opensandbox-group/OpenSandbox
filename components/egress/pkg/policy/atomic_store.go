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

package policy

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

// FileMutationState describes the authoritative policy path, not temporary files.
// A caller must not infer FileUnchanged merely because rename returned an error.
type FileMutationState uint8

const (
	// FileUnchanged means no rename or unlink of the authoritative path was attempted.
	FileUnchanged FileMutationState = iota
	// FileCommitted means the operation and its parent-directory fsync completed.
	FileCommitted
	// FileUnknown means publication was attempted, or its durability/close failed.
	FileUnknown
)

// AtomicTempPrefix and AtomicTempSuffix name the staging files the mutation
// ops create next to the authoritative path before the rename. The pattern is
// part of the persistence contract: owners pre-scanning for staged leftovers
// of an interrupted write match on exactly this pair.
const (
	AtomicTempPrefix = ".policy-"
	AtomicTempSuffix = ".tmp"
)

// FileMutationError identifies an I/O phase without including policy contents.
type FileMutationError struct {
	Phase string
	Err   error
}

func (e *FileMutationError) Error() string {
	return fmt.Sprintf("atomic policy file %s: %v", e.Phase, e.Err)
}
func (e *FileMutationError) Unwrap() error { return e.Err }

func atomicFileError(phase string, err error) error {
	return &FileMutationError{Phase: phase, Err: err}
}

// PolicyFileSnapshot is an opaque, exact image of one policy path. It preserves
// absence, bytes, permission bits and ownership plus SELinux comparison state;
// it is not a reserialized policy or a copy of integrity attributes.
type PolicyFileSnapshot struct {
	path     string
	exists   bool
	data     []byte
	mode     fs.FileMode
	uid      int
	gid      int
	security policyFileSecurity
}

// policyFileSecurity records only comparison/presence information. Integrity
// attribute payloads are never retained or copied onto replacement contents.
type policyFileSecurity struct {
	selinux  string
	ima, evm bool
}

// Exists reports whether the snapshot captured a present file. A false return
// means Restore will durably unlink the path.
func (s *PolicyFileSnapshot) Exists() bool { return s != nil && s.exists }

// Data returns the captured file bytes, or nil when the snapshot captured
// absence. The returned slice must be treated as read-only.
func (s *PolicyFileSnapshot) Data() []byte {
	if s == nil {
		return nil
	}
	return s.data
}

// NewFileSnapshot builds an opaque snapshot from exact image bytes — the
// seam for owners that substitute the atomic file store (and its runtime
// tests). exists=false produces the absent image, which Restore answers by
// durably unlinking the path.
func NewFileSnapshot(path string, data []byte, exists bool) *PolicyFileSnapshot {
	return &PolicyFileSnapshot{path: filepath.Clean(path), exists: exists, data: data}
}
