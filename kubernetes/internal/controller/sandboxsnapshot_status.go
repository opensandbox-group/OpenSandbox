// Copyright 2025 Alibaba Group Holding Ltd.
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

package controller

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func setSnapshotConditionInStatus(
	status *sandboxv1alpha1.SandboxSnapshotStatus,
	conditionType sandboxv1alpha1.SandboxSnapshotConditionType,
	conditionStatus string,
	reason string,
	message string,
) {
	filtered := make([]sandboxv1alpha1.SandboxSnapshotCondition, 0, len(status.Conditions))
	found := false
	for _, cond := range status.Conditions {
		if cond.Type != conditionType {
			filtered = append(filtered, cond)
			continue
		}
		found = true
		if conditionStatus == sandboxv1alpha1.ConditionFalse {
			continue
		}
		cond.Status = conditionStatus
		cond.Reason = reason
		cond.Message = message
		cond.LastTransitionTime = ptrToTime(metav1.Now())
		filtered = append(filtered, cond)
	}
	if !found && conditionStatus == sandboxv1alpha1.ConditionTrue {
		filtered = append(filtered, sandboxv1alpha1.SandboxSnapshotCondition{
			Type:               conditionType,
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: ptrToTime(metav1.Now()),
		})
	}
	status.Conditions = filtered
}

func applySnapshotPhaseConditions(status *sandboxv1alpha1.SandboxSnapshotStatus, failureReason string, failureMessage string) {
	switch status.Phase {
	case sandboxv1alpha1.SandboxSnapshotPhasePending:
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionReady, sandboxv1alpha1.ConditionFalse, "Pending", "Snapshot request accepted")
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionFailed, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionReady, sandboxv1alpha1.ConditionFalse, "Committing", "Snapshot commit job is running")
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionFailed, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.SandboxSnapshotPhaseSucceed:
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionReady, sandboxv1alpha1.ConditionTrue, "SnapshotReady", "Snapshot is ready")
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionFailed, sandboxv1alpha1.ConditionFalse, "", "")
	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionReady, sandboxv1alpha1.ConditionFalse, "Failed", "Snapshot failed")
		setSnapshotConditionInStatus(status, sandboxv1alpha1.SandboxSnapshotConditionFailed, sandboxv1alpha1.ConditionTrue, failureReason, failureMessage)
	}
}

func isTerminalSnapshotPhase(phase sandboxv1alpha1.SandboxSnapshotPhase) bool {
	return phase == sandboxv1alpha1.SandboxSnapshotPhaseSucceed || phase == sandboxv1alpha1.SandboxSnapshotPhaseFailed
}

func (r *SandboxSnapshotReconciler) ackGeneration(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		latest.Status.ObservedGeneration = latest.Generation
		if latest.Status.Phase == "" {
			latest.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhasePending
		}
		applySnapshotPhaseConditions(&latest.Status, "", "")
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) persistResolvedData(
	ctx context.Context,
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	sourcePodName, sourceNodeName string,
	containers []sandboxv1alpha1.ContainerSnapshot,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		latest.Status.SourcePodName = sourcePodName
		latest.Status.SourceNodeName = sourceNodeName
		latest.Status.Containers = containers
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) updateSnapshotStatus(
	ctx context.Context,
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	phase sandboxv1alpha1.SandboxSnapshotPhase,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		if isTerminalSnapshotPhase(latest.Status.Phase) && latest.Status.Phase != phase {
			return nil
		}
		latest.Status.Phase = phase
		applySnapshotPhaseConditions(&latest.Status, reason, message)
		return r.Status().Update(ctx, latest)
	})
}

func ptrToInt64(v int64) *int64            { return &v }
func ptrToInt32(v int32) *int32            { return &v }
func ptrToBool(v bool) *bool               { return &v }
func ptrToTime(v metav1.Time) *metav1.Time { return &v }

func (r *SandboxSnapshotReconciler) getCommitJobTimeout() time.Duration {
	if r.CommitJobTimeout > 0 {
		return r.CommitJobTimeout
	}
	return DefaultCommitJobTimeout
}
