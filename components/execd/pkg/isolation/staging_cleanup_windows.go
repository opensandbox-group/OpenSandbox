//go:build windows

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

package isolation

// cleanupStaging removes the staging entry, if this write still owns it.
//
// Windows refuses to unlink a file that is still open, so the descriptor goes
// first. That leaves a narrower ordering window than the POSIX path: a session
// could in principle recycle the inode number between the close and the check.
// Isolated sessions are a Linux-runtime feature, so this platform only has to
// keep the package compiling and cleaning up its own temp files.
func cleanupStaging(fsys stagingFS, staging *stagingFile) {
	_ = staging.file.Close()
	if staging.ownsEntry(fsys) {
		_ = fsys.Remove(staging.name)
	}
}

// closeStagingBeforeCleanup reports whether the platform has to close the
// descriptor before the entry can be removed.
func closeStagingBeforeCleanup() bool { return true }
