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

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

// reconcileDeletion owns the terminating window. No allocation, scaling,
// pause/resume or runtime status reconciliation may start new work here.
func (r *BatchSandboxReconciler) reconcileDeletion(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, finalizerTaskCleanup) {
		r.deleteTaskScheduler(ctx, sandbox)
		return ctrl.Result{}, nil
	}

	poolStrategy := strategy.NewPoolStrategy(sandbox)
	pods, err := r.listPods(ctx, poolStrategy, sandbox)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe pods during deletion: %w", err)
	}
	// Recovery after a controller restart must confirm executor state before
	// task cleanup can hand a pod back to the Pool or terminate it.
	scheduler, err := r.getTaskScheduler(ctx, sandbox, pods)
	if err != nil {
		return ctrl.Result{}, err
	}
	scheduler.StopTask()
	if err := scheduler.Schedule(); err != nil {
		return ctrl.Result{}, fmt.Errorf("clean up tasks: %w", err)
	}
	if len(r.getTasksCleanupUnfinished(sandbox, scheduler)) > 0 {
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Pooled pods are recycled by the Pool controller. Removing task-cleanup
	// hands back any allocations not already requested through alloc-release.
	// Direct pods must disappear before this (last) cleanup finalizer is removed.
	if !poolStrategy.IsPooledMode() && len(pods) > 0 {
		for _, pod := range pods {
			if pod.DeletionTimestamp == nil {
				if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, fmt.Errorf("delete pod %s: %w", pod.Name, err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}
	if err := utils.UpdateFinalizer(r.Client, sandbox, utils.RemoveFinalizerOpType, finalizerTaskCleanup); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	r.deleteTaskScheduler(ctx, sandbox)
	return ctrl.Result{}, nil
}

func batchSandboxDeletionPolicy(batchSbx *sandboxv1alpha1.BatchSandbox) metav1.DeletionPropagation {
	if batchSbx.Spec.TaskTemplate != nil {
		// Foreground GC would terminate the executor before task cleanup finishes.
		return metav1.DeletePropagationBackground
	}
	return metav1.DeletePropagationForeground
}
