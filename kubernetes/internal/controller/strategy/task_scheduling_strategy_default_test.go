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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func TestDefaultTaskSchedulingStrategy_ValidateShardTaskPatches(t *testing.T) {
	tests := []struct {
		name     string
		batchSbx *sandboxv1alpha1.BatchSandbox
		wantErr  bool
	}{
		{
			name: "no shard task patches - valid",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
				},
			},
			wantErr: false,
		},
		{
			name: "valid shard task patch with string args",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
					ShardTaskPatches: []runtime.RawExtension{
						{Raw: []byte(`{"spec":{"process":{"command":["sleep"],"args":["3600"]}}}`)},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid shard task patch - args as integer instead of string array",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
					ShardTaskPatches: []runtime.RawExtension{
						{Raw: []byte(`{"spec":{"process":{"args":3600}}}`)},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid shard task patch - malformed JSON",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
					ShardTaskPatches: []runtime.RawExtension{
						{Raw: []byte(`{"invalid json`)},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "second patch invalid - returns error with correct index",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
					ShardTaskPatches: []runtime.RawExtension{
						{Raw: []byte(`{"spec":{"process":{"args":["valid"]}}}`)},
						{Raw: []byte(`{"spec":{"process":{"args":3600}}}`)},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewDefaultTaskSchedulingStrategy(tt.batchSbx)
			err := s.ValidateShardTaskPatches()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateShardTaskPatches() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultTaskSchedulingStrategy_NeedTaskScheduling(t *testing.T) {
	tests := []struct {
		name     string
		batchSbx *sandboxv1alpha1.BatchSandbox
		want     bool
	}{
		{
			name: "with task template",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{},
				},
			},
			want: true,
		},
		{
			name: "without task template",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					TaskTemplate: nil,
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strategy := NewDefaultTaskSchedulingStrategy(tt.batchSbx)
			if got := strategy.NeedTaskScheduling(); got != tt.want {
				t.Errorf("DefaultTaskSchedulingStrategy.NeedTaskScheduling() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultTaskSchedulingStrategy_getTaskSpec(t *testing.T) {
	type args struct {
		batchSbx *sandboxv1alpha1.BatchSandbox
		idx      int
	}
	tests := []struct {
		name    string
		args    args
		want    *api.Task
		wantErr bool
	}{
		{
			name: "basic task spec without patches",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-bs",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
							Spec: sandboxv1alpha1.TaskSpec{
								Process: &sandboxv1alpha1.ProcessTask{
									Command: []string{"echo", "hello"},
								},
							},
						},
					},
				},
				idx: 0,
			},
			want: &api.Task{
				Name: "test-bs-0",
				Process: &api.Process{
					Command: []string{"echo", "hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "task spec with shard patch",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-bs",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
							Spec: sandboxv1alpha1.TaskSpec{
								Process: &sandboxv1alpha1.ProcessTask{
									Command: []string{"echo", "hello"},
								},
							},
						},
						ShardTaskPatches: []runtime.RawExtension{
							{
								Raw: []byte(`{"spec":{"process":{"command":["echo","world"]}}}`),
							},
						},
					},
				},
				idx: 0,
			},
			want: &api.Task{
				Name: "test-bs-0",
				Process: &api.Process{
					Command: []string{"echo", "world"},
				},
			},
			wantErr: false,
		},
		{
			name: "task spec with invalid patch",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-bs",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
							Spec: sandboxv1alpha1.TaskSpec{
								Process: &sandboxv1alpha1.ProcessTask{
									Command: []string{"echo", "hello"},
								},
							},
						},
						ShardTaskPatches: []runtime.RawExtension{
							{
								Raw: []byte(`{"invalid json`),
							},
						},
					},
				},
				idx: 0,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "task spec with index out of range patch",
			args: args{
				batchSbx: &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-bs",
						Namespace: "default",
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{
							Spec: sandboxv1alpha1.TaskSpec{
								Process: &sandboxv1alpha1.ProcessTask{
									Command: []string{"echo", "hello"},
								},
							},
						},
						ShardTaskPatches: []runtime.RawExtension{
							{
								Raw: []byte(`{"spec":{"process":{"command":["echo","world"]}}}`),
							},
						},
					},
				},
				idx: 1,
			},
			want: &api.Task{
				Name: "test-bs-1",
				Process: &api.Process{
					Command: []string{"echo", "hello"},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strategy := NewDefaultTaskSchedulingStrategy(tt.args.batchSbx)
			got, err := strategy.getTaskSpec(tt.args.idx)
			if (err != nil) != tt.wantErr {
				t.Errorf("DefaultTaskSchedulingStrategy.getTaskSpec() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.Name != tt.want.Name {
					t.Errorf("DefaultTaskSchedulingStrategy.getTaskSpec() name = %v, want %v", got.Name, tt.want.Name)
				}
				if !reflect.DeepEqual(got.Process, tt.want.Process) {
					t.Errorf("DefaultTaskSchedulingStrategy.getTaskSpec() spec = %v, want %v", got.Process, tt.want.Process)
				}
			}
		})
	}
}
