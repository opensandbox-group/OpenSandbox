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

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsolationStatsSnapshotUpperUsageValidity(t *testing.T) {
	runner := newTestRunner(t)
	runner.ctrl.isolatedSessionMap.Store("active-session", struct{}{})
	defer runner.ctrl.isolatedSessionMap.Delete("active-session")

	check := func(wantBytes int64, wantValid bool) {
		t.Helper()
		snapshot := runner.statsSnapshot()
		if snapshot.ActiveSessions != 1 {
			t.Fatalf("active sessions = %d, want 1", snapshot.ActiveSessions)
		}
		if snapshot.UpperUsageValid != wantValid {
			t.Fatalf("upper usage valid = %v, want %v", snapshot.UpperUsageValid, wantValid)
		}
		if wantValid && snapshot.UpperUsageBytes != wantBytes {
			t.Fatalf("upper usage = %d, want %d", snapshot.UpperUsageBytes, wantBytes)
		}
	}

	// Zero is a valid observation when no upper has been allocated.
	check(0, true)

	id, upperDir, _, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		runner.upperMgr.Release(id)
		if err := runner.upperMgr.Remove(id); err != nil {
			t.Errorf("remove upper: %v", err)
		}
	}()
	if err := os.WriteFile(filepath.Join(upperDir, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(int64(len("payload")), true)

	// An in-use upper that disappears must not masquerade as 0 bytes.
	if err := os.RemoveAll(upperDir); err != nil {
		t.Fatal(err)
	}
	check(0, false)

	if err := os.MkdirAll(upperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upperDir, "restored"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(int64(len("restored")), true)

	// Missing released uppers remain allowable GC residue.
	runner.upperMgr.Release(id)
	if err := os.RemoveAll(upperDir); err != nil {
		t.Fatal(err)
	}
	check(0, true)
}
