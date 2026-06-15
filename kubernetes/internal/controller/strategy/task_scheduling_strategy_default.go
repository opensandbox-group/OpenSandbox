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

package strategy

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/util/strategicpatch"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// DefaultTaskSchedulingStrategy implements the default task scheduling strategy.
type DefaultTaskSchedulingStrategy struct {
	*sandboxv1alpha1.BatchSandbox
}

// NewDefaultTaskSchedulingStrategy creates a new default task scheduling strategy.
func NewDefaultTaskSchedulingStrategy(batchSbx *sandboxv1alpha1.BatchSandbox) *DefaultTaskSchedulingStrategy {
	return &DefaultTaskSchedulingStrategy{
		BatchSandbox: batchSbx,
	}
}

// NeedTaskScheduling determines whether task scheduling is needed based on TaskTemplate.
func (s *DefaultTaskSchedulingStrategy) NeedTaskScheduling() bool {
	return s.Spec.TaskTemplate != nil
}

// ValidateShardTaskPatches checks that every shardTaskPatch can be successfully
// merged into a zero-value TaskTemplateSpec. It returns the first error encountered,
// including the patch index and the raw patch bytes to aid diagnosis.
func (s *DefaultTaskSchedulingStrategy) ValidateShardTaskPatches() error {
	if len(s.Spec.ShardTaskPatches) == 0 {
		return nil
	}
	// Zero-value base: we check structural correctness in isolation, not against any
	// specific user template, so a bad patch is caught even when TaskTemplate is nil.
	zeroBytes, err := json.Marshal(&sandboxv1alpha1.TaskTemplateSpec{})
	if err != nil {
		return fmt.Errorf("batchsandbox: failed to marshal zero TaskTemplateSpec: %w", err)
	}
	for i, patch := range s.Spec.ShardTaskPatches {
		// Truncate patch in error messages to avoid persisting large blobs in status conditions.
		patchSummary := patch.Raw
		if len(patchSummary) > 200 {
			patchSummary = append(patchSummary[:200], []byte("...(truncated)")...)
		}
		modified, mergeErr := strategicpatch.StrategicMergePatch(zeroBytes, patch.Raw, &sandboxv1alpha1.TaskTemplateSpec{})
		if mergeErr != nil {
			return fmt.Errorf("batchsandbox: shardTaskPatches[%d] failed schema validation: patch %s, err %w", i, patchSummary, mergeErr)
		}
		if err = json.Unmarshal(modified, &sandboxv1alpha1.TaskTemplateSpec{}); err != nil {
			return fmt.Errorf("batchsandbox: shardTaskPatches[%d] produced invalid TaskTemplateSpec: patch %s, err %w", i, patchSummary, err)
		}
	}
	return nil
}

// GenerateTaskSpecs generates task specifications for all replicas.
func (s *DefaultTaskSchedulingStrategy) GenerateTaskSpecs() ([]*api.Task, error) {
	ret := make([]*api.Task, *s.Spec.Replicas)
	for idx := range int(*s.Spec.Replicas) {
		task, err := s.getTaskSpec(idx)
		if err != nil {
			return ret, err
		}
		ret[idx] = task
	}
	return ret, nil
}

// getTaskSpec generates a single task specification for the given index.
// It applies ShardTaskPatches if available, otherwise uses the base TaskTemplate.
func (s *DefaultTaskSchedulingStrategy) getTaskSpec(idx int) (*api.Task, error) {
	task := &api.Task{
		Name: fmt.Sprintf("%s-%d", s.Name, idx),
	}
	if len(s.Spec.ShardTaskPatches) > 0 && idx < len(s.Spec.ShardTaskPatches) {
		taskTemplate := s.Spec.TaskTemplate.DeepCopy()
		cloneBytes, _ := json.Marshal(taskTemplate)
		patch := s.Spec.ShardTaskPatches[idx]
		modified, err := strategicpatch.StrategicMergePatch(cloneBytes, patch.Raw, &sandboxv1alpha1.TaskTemplateSpec{})
		if err != nil {
			return nil, fmt.Errorf("batchsandbox: failed to merge patch raw %s, idx %d, err %w", patch.Raw, idx, err)
		}
		newTaskTemplate := &sandboxv1alpha1.TaskTemplateSpec{}
		if err = json.Unmarshal(modified, newTaskTemplate); err != nil {
			return nil, fmt.Errorf("batchsandbox: failed to unmarshal %s to TaskTemplateSpec, idx %d, err %w", modified, idx, err)
		}
		task.Process = &api.Process{
			Command:        newTaskTemplate.Spec.Process.Command,
			Args:           newTaskTemplate.Spec.Process.Args,
			Env:            newTaskTemplate.Spec.Process.Env,
			WorkingDir:     newTaskTemplate.Spec.Process.WorkingDir,
			TimeoutSeconds: s.Spec.TaskTemplate.Spec.TimeoutSeconds,
		}
	} else if s.Spec.TaskTemplate != nil && s.Spec.TaskTemplate.Spec.Process != nil {
		task.Process = &api.Process{
			Command:        s.Spec.TaskTemplate.Spec.Process.Command,
			Args:           s.Spec.TaskTemplate.Spec.Process.Args,
			Env:            s.Spec.TaskTemplate.Spec.Process.Env,
			WorkingDir:     s.Spec.TaskTemplate.Spec.Process.WorkingDir,
			TimeoutSeconds: s.Spec.TaskTemplate.Spec.TimeoutSeconds,
		}
	}
	return task, nil
}
