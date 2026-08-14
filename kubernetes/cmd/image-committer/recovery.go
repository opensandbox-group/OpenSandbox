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
	"fmt"
	"os"
	"sync"
)

// snapshotRecovery owns the actions needed to make a partially checkpointed
// source workload runnable again. It is shared with the signal handler, so
// taking the pending actions and clearing them must be atomic.
type snapshotRecovery struct {
	mu                   sync.Mutex
	pausedContainerIDs   []string
	resumeVirtualMachine func() error
}

func (r *snapshotRecovery) trackPausedContainer(containerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pausedContainerIDs = append(r.pausedContainerIDs, containerID)
}

func (r *snapshotRecovery) setVirtualMachineResume(resume func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumeVirtualMachine = resume
}

// disarm intentionally abandons recovery when the controller owns deletion of
// a successfully snapshotted, frozen source Pod.
func (r *snapshotRecovery) disarm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pausedContainerIDs = nil
	r.resumeVirtualMachine = nil
}

func (r *snapshotRecovery) resumeSource() error {
	r.mu.Lock()
	pausedContainerIDs := append([]string(nil), r.pausedContainerIDs...)
	resumeVirtualMachine := r.resumeVirtualMachine
	r.pausedContainerIDs = nil
	r.resumeVirtualMachine = nil
	r.mu.Unlock()

	var resumeErrors []error
	failedContainerIDs := make(map[string]struct{})
	if len(pausedContainerIDs) > 0 {
		fmt.Println("\n=== Cleanup: Resuming paused source containers ===")
	}
	for i := len(pausedContainerIDs) - 1; i >= 0; i-- {
		containerID := pausedContainerIDs[i]
		if err := resumeContainer(containerID); err != nil {
			failedContainerIDs[containerID] = struct{}{}
			resumeErrors = append(resumeErrors, fmt.Errorf("resume container %s: %w", containerID, err))
		}
	}
	virtualMachineResumeFailed := false
	if resumeVirtualMachine != nil {
		if err := resumeVirtualMachine(); err != nil {
			virtualMachineResumeFailed = true
			resumeErrors = append(resumeErrors, fmt.Errorf("resume virtual machine: %w", err))
		}
	}

	// Keep only failed actions armed so the caller's final recovery pass can
	// retry transient containerd or QMP failures without repeating successes.
	if len(failedContainerIDs) > 0 || virtualMachineResumeFailed {
		r.mu.Lock()
		failed := make([]string, 0, len(failedContainerIDs))
		for _, containerID := range pausedContainerIDs {
			if _, ok := failedContainerIDs[containerID]; ok {
				failed = append(failed, containerID)
			}
		}
		r.pausedContainerIDs = append(failed, r.pausedContainerIDs...)
		if virtualMachineResumeFailed && r.resumeVirtualMachine == nil {
			r.resumeVirtualMachine = resumeVirtualMachine
		}
		r.mu.Unlock()
	}
	return errors.Join(resumeErrors...)
}

func recoverSnapshotSource(recovery *snapshotRecovery) {
	if err := recovery.resumeSource(); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: failed to recover snapshot source: %v\n", err)
	}
}
