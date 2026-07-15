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

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

const (
	// SandboxSnapshotFinalizer is the finalizer for SandboxSnapshot cleanup
	SandboxSnapshotFinalizer = "sandboxsnapshot.sandbox.opensandbox.io/cleanup"

	// DefaultCommitJobTimeout is the default timeout for commit jobs
	DefaultCommitJobTimeout = 10 * time.Minute

	// DefaultCommitJobBackoffLimit bounds commit/push retries so stable failures
	// surface as snapshot failures within the pause/resume e2e timeout window while
	// still tolerating a few transient job failures.
	DefaultCommitJobBackoffLimit int32 = 3

	DefaultTTLSecondsAfterFinished = 300

	// CommitJobContainerName is the container name in commit job
	CommitJobContainerName = "commit"

	// ContainerdSocketPath is the default containerd socket path
	ContainerdSocketPath = "/var/run/containerd/containerd.sock"

	// LabelSandboxSnapshotName is the label key for sandbox snapshot name
	LabelSandboxSnapshotName = "sandbox.opensandbox.io/sandbox-snapshot-name"
)

// SandboxSnapshotReconciler reconciles a SandboxSnapshot object.
// Pure atomic capability: reads BatchSandbox via spec.sandboxName, finds Pod,
// creates commit Job to commit+push container images, reports status.
// No business logic (no scaling, no pool, no resume).
type SandboxSnapshotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// ImageCommitterImage is the image for image-committer (uses nerdctl to commit/push container images)
	ImageCommitterImage string

	// ContainerdSocketPath is containerd socket path for image-committer (nerdctl --address)
	ContainerdSocketPath string

	// CommitJobTimeout is the timeout for commit jobs (default: 10 minutes)
	CommitJobTimeout time.Duration

	// SnapshotRegistry is the OCI registry for snapshot images (from Controller Manager startup params)
	SnapshotRegistry string

	// SnapshotPushSecret is the K8s Secret name for pushing to registry (from Controller Manager startup params)
	SnapshotPushSecret string

	// SnapshotRegistryInsecure controls whether image-committer uses insecure registry mode.
	SnapshotRegistryInsecure bool
}

// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := logf.FromContext(ctx)
	start := time.Now()
	defer func() {
		log.Info("Reconcile finished", "duration", time.Since(start).String(), "requeueAfter", result.RequeueAfter.String(), "error", retErr)
	}()

	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !snapshot.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, snapshot)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.AddFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	// ACK generation immediately to prevent re-entry
	generation := snapshot.Generation
	if generation > snapshot.Status.ObservedGeneration {
		if err := r.ackGeneration(ctx, snapshot); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after ACK
		if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Dispatch by phase
	switch snapshot.Status.Phase {
	case "", sandboxv1alpha1.SandboxSnapshotPhasePending:
		return r.handlePending(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		return r.handleCommitting(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseSucceed:
		// Succeed: nothing more to do, BatchSandbox Controller handles completion
		return ctrl.Result{}, nil
	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		// Failed: wait for BatchSandbox Controller to handle recovery
		return ctrl.Result{}, nil
	default:
		log.Info("Unknown phase, treating as Pending", "phase", snapshot.Status.Phase)
		return r.handlePending(ctx, snapshot)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SandboxSnapshot{}).
		Owns(&batchv1.Job{}).
		Named("sandboxsnapshot").
		Complete(r)
}
