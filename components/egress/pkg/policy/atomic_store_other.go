//go:build !linux

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

import "errors"

// AtomicPolicyFile is unsupported outside the experimental Linux runtime.
type AtomicPolicyFile struct{}

func atomicPolicyPlatformError() error {
	return atomicFileError("validate", errors.New("atomic policy storage requires Linux"))
}

func NewAtomicPolicyFile(string) (*AtomicPolicyFile, error) { return nil, atomicPolicyPlatformError() }
func (*AtomicPolicyFile) Snapshot() (*PolicyFileSnapshot, error) {
	return nil, atomicPolicyPlatformError()
}
func (*AtomicPolicyFile) Save(*NetworkPolicy) (FileMutationState, error) {
	return FileUnchanged, atomicPolicyPlatformError()
}
func (*AtomicPolicyFile) SaveRaw([]byte) (FileMutationState, error) {
	return FileUnchanged, atomicPolicyPlatformError()
}
func (*AtomicPolicyFile) SaveRawNonDurable([]byte) (FileMutationState, error) {
	return FileUnchanged, atomicPolicyPlatformError()
}
func (*AtomicPolicyFile) Restore(*PolicyFileSnapshot) (FileMutationState, error) {
	return FileUnchanged, atomicPolicyPlatformError()
}
func (*AtomicPolicyFile) Delete() (FileMutationState, error) {
	return FileUnchanged, atomicPolicyPlatformError()
}
