// Copyright 2026 The OpenSandbox Authors

//go:build !windows

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

package runtime

import "testing"

func TestIsolationStatsSnapshotActiveSessions(t *testing.T) {
	runner := newTestRunner(t)
	runner.ctrl.isolatedSessionMap.Store("active-session", struct{}{})
	defer runner.ctrl.isolatedSessionMap.Delete("active-session")

	snapshot := runner.statsSnapshot()
	if snapshot.ActiveSessions != 1 {
		t.Fatalf("active sessions = %d, want 1", snapshot.ActiveSessions)
	}
}
