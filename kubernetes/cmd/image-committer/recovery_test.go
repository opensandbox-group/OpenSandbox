// Copyright 2026 Alibaba Group Holding Ltd.
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

package main

import (
	"errors"
	"testing"
)

func TestSnapshotRecoveryResumesVirtualMachineOnce(t *testing.T) {
	recovery := &snapshotRecovery{}
	resumeCalls := 0
	recovery.setVirtualMachineResume(func() error {
		resumeCalls++
		return nil
	})

	if err := recovery.resumeSource(); err != nil {
		t.Fatal(err)
	}
	if err := recovery.resumeSource(); err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 1 {
		t.Fatalf("expected one VM resume, got %d", resumeCalls)
	}
}

func TestSnapshotRecoveryDisarmLeavesSourceFrozen(t *testing.T) {
	recovery := &snapshotRecovery{}
	resumeCalls := 0
	recovery.setVirtualMachineResume(func() error {
		resumeCalls++
		return nil
	})

	recovery.disarm()
	if err := recovery.resumeSource(); err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 0 {
		t.Fatalf("expected disarmed recovery not to resume VM, got %d calls", resumeCalls)
	}
}

func TestSnapshotRecoveryRetriesFailedVirtualMachineResume(t *testing.T) {
	recovery := &snapshotRecovery{}
	resumeCalls := 0
	recovery.setVirtualMachineResume(func() error {
		resumeCalls++
		if resumeCalls == 1 {
			return errors.New("temporary QMP failure")
		}
		return nil
	})

	if err := recovery.resumeSource(); err == nil {
		t.Fatal("expected the first resume to fail")
	}
	if err := recovery.resumeSource(); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if resumeCalls != 2 {
		t.Fatalf("expected two VM resume attempts, got %d", resumeCalls)
	}
}
