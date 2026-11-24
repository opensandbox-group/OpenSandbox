//go:build !windows

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
// POSIX can unlink a file that is still open, so the ownership check and the
// unlink both run while the descriptor pins the inode: the allocator cannot
// recycle the number in between and make a session's fresh file look like ours.
// The descriptor is closed last.
func cleanupStaging(fsys stagingFS, staging *stagingFile) {
	if staging.ownsEntry(fsys) {
		_ = fsys.Remove(staging.name)
	}
	_ = staging.file.Close()
}

// closeStagingBeforeCleanup reports whether the platform has to close the
// descriptor before the entry can be removed.
func closeStagingBeforeCleanup() bool { return false }
