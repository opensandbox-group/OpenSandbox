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

package recycle

import (
	"testing"

	"github.com/stretchr/testify/assert"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestNewHandler(t *testing.T) {
	tests := []struct {
		name        string
		pool        *sandboxv1alpha1.Pool
		wantErr     bool
		wantHandler interface{}
	}{
		{
			name:        "DefaultIsDelete",
			pool:        &sandboxv1alpha1.Pool{},
			wantHandler: &DeleteRecycler{},
		},
		{
			name: "Noop",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeNoop,
					},
				},
			},
			wantHandler: &NoopRecycler{},
		},
		{
			name: "Delete",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeDelete,
					},
				},
			},
			wantHandler: &DeleteRecycler{},
		},
		{
			name: "Restart_NilConfig",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: sandboxv1alpha1.RecycleTypeRestart,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "UnknownType_FallbackToDelete",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					RecycleStrategy: &sandboxv1alpha1.RecycleStrategy{
						Type: "unknown",
					},
				},
			},
			wantHandler: &DeleteRecycler{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := NewHandler(nil, nil, tt.pool)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, h)
				return
			}
			assert.NoError(t, err)
			assert.IsType(t, tt.wantHandler, h)
		})
	}
}
