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

package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	snapshotcontract "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

func isOwnedPauseSnapshot(snapshot *sandboxv1alpha1.SandboxSnapshot, bs *sandboxv1alpha1.BatchSandbox) bool {
	owner := metav1.GetControllerOfNoCopy(snapshot)
	return bs.UID != "" && owner != nil && owner.Kind == "BatchSandbox" &&
		owner.APIVersion == sandboxv1alpha1.SchemeBuilder.GroupVersion.String() && owner.Name == bs.Name && owner.UID == bs.UID &&
		snapshot.Namespace == bs.Namespace && snapshot.Name == internalPauseSnapshotName(bs.Name) && snapshot.Spec.SandboxName == bs.Name
}

func hasRecreatePolicy(bs *sandboxv1alpha1.BatchSandbox) bool {
	return len(bs.Spec.RecreateContainersOnResume) != 0 || len(bs.Status.RecreateContainersOnResume) != 0
}

// rootfsSnapshotTargets returns the template containers that retain their writable layers.
func rootfsSnapshotTargets(bs *sandboxv1alpha1.BatchSandbox) ([]corev1.Container, error) {
	spec := &bs.Spec
	original := bs.Status.RecreateContainersOnResume
	if len(original) == 0 || len(original) != len(spec.RecreateContainersOnResume) {
		return nil, fmt.Errorf("recreateContainersOnResume must match the creation-time policy")
	}
	for _, entry := range spec.RecreateContainersOnResume {
		if !slices.Contains(original, entry) {
			return nil, fmt.Errorf("recreateContainersOnResume must match the creation-time policy")
		}
	}
	if spec.Template == nil || spec.PoolRef != "" || len(spec.ShardPatches) != 0 || spec.Replicas == nil || *spec.Replicas != supportedPauseReplicas {
		return nil, fmt.Errorf("recreateContainersOnResume requires a non-pooled single-replica template without shard patches")
	}
	pins := make(map[string]string, len(spec.RecreateContainersOnResume))
	for _, entry := range spec.RecreateContainersOnResume {
		if entry.Name == "" || pins[entry.Name] != "" {
			return nil, fmt.Errorf("invalid or duplicate recreate container name %q", entry.Name)
		}
		parsed, err := reference.Parse(entry.Image)
		pinned, ok := parsed.(reference.Canonical)
		if err != nil || !ok {
			return nil, fmt.Errorf("recreate container %q requires an image pinned by manifest digest", entry.Name)
		}
		if _, err := snapshotcontract.ParseSHA256Digest(pinned.Digest().String()); err != nil {
			return nil, fmt.Errorf("invalid digest for recreate container %q: %w", entry.Name, err)
		}
		pins[entry.Name] = entry.Image
	}
	containers := spec.Template.Spec.Containers
	targets := make([]corev1.Container, 0, len(containers))
	matched := 0
	for _, container := range containers {
		pin, recreate := pins[container.Name]
		if !recreate {
			targets = append(targets, container)
			continue
		}
		if container.Image != pin {
			return nil, fmt.Errorf("template image of recreate container %q does not match its creation-time pin", container.Name)
		}
		matched++
	}
	if matched != len(pins) {
		return nil, fmt.Errorf("recreate containers must exist in the Pod template")
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one container must retain its root filesystem")
	}
	return targets, nil
}

func validateRecreateSnapshot(bs *sandboxv1alpha1.BatchSandbox, snapshot *sandboxv1alpha1.SandboxSnapshot) error {
	if !isOwnedPauseSnapshot(snapshot, bs) || snapshot.Status.Format != sandboxv1alpha1.SandboxSnapshotFormatRootfsV1 {
		return fmt.Errorf("recreateContainersOnResume requires the BatchSandbox's internal rootfs-v1 snapshot")
	}
	targets, err := rootfsSnapshotTargets(bs)
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(targets))
	for _, c := range targets {
		expected[c.Name] = struct{}{}
	}
	for _, c := range snapshot.Status.Containers {
		if _, ok := expected[c.ContainerName]; !ok {
			return fmt.Errorf("unexpected or duplicate snapshot result for container %q", c.ContainerName)
		}
		if _, err := snapshotcontract.ImmutableImageReference(c.ImageURI, c.ImageDigest); err != nil {
			return fmt.Errorf("invalid snapshot result for container %q: %w", c.ContainerName, err)
		}
		delete(expected, c.ContainerName)
	}
	if len(expected) != 0 {
		return fmt.Errorf("snapshot is missing results for one or more template containers")
	}
	return nil
}

func validateSourceContainerPins(bs *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) error {
	if !metav1.IsControlledBy(pod, bs) || pod.DeletionTimestamp != nil {
		return fmt.Errorf("source Pod is not an active Pod controlled by the BatchSandbox")
	}
	for _, entry := range bs.Spec.RecreateContainersOnResume {
		matches := false
		for _, container := range pod.Spec.Containers {
			if container.Name == entry.Name && container.Image == entry.Image {
				matches = true
				break
			}
		}
		if !matches {
			return fmt.Errorf("source Pod does not use the creation-time pin for container %q", entry.Name)
		}
	}
	return nil
}

// dispatchRecreatePause waits for provisioning to produce a running source Pod,
// but acknowledges a terminal source failure without starting the pause workflow.
func (r *BatchSandboxReconciler) dispatchRecreatePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, bool, error) {
	_, pauseErr := rootfsSnapshotTargets(bs)
	reason := "InvalidRecreatePolicy"
	if pauseErr == nil && bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhaseFailed &&
		hasTerminalPodFailureCondition(bs.Status.Conditions) {
		reason = "SourcePodFailed"
		pauseErr = fmt.Errorf("source Pod failed before it could be paused")
	}
	if pauseErr == nil {
		pods, err := r.listPods(ctx, strategy.NewPoolStrategy(bs), bs)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		for _, pod := range pods {
			if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
				pauseErr = validateRecreateSourcePod(bs, pod)
				if pauseErr == nil {
					result, err := r.handlePause(ctx, bs)
					return result, true, err
				}
				break
			}
		}
		if pauseErr == nil {
			return ctrl.Result{}, false, nil
		}
	}
	bs.Status.PauseObservedGeneration = bs.Generation
	setConditionInStatus(&bs.Status, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, reason, pauseErr.Error())
	return ctrl.Result{}, true, r.Status().Update(ctx, bs)
}

func validateRecreateSourcePod(bs *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) error {
	contract, err := snapshotcontract.ContractFromPod(pod)
	if err != nil {
		return err
	}
	if contract.Provider != snapshotcontract.ProviderRootfs {
		return fmt.Errorf("recreateContainersOnResume only supports rootfs-v1")
	}
	return validateSourceContainerPins(bs, pod)
}

func hasImagePullSecret(template *corev1.PodTemplateSpec, name string) bool {
	return slices.Contains(template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
}
