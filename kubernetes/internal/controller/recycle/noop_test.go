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
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/assert"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestNoopRecycler(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
	}{
		{name: "NilPod", pod: nil},
		{name: "WithPod", pod: &corev1.Pod{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := NewNoopRecycler()
			status, err := n.TryRecycle(context.Background(), &sandboxv1alpha1.Pool{}, tt.pod, &Spec{ID: "sbx1"})
			assert.NoError(t, err)
			assert.Equal(t, StateSucceeded, status.State)
			assert.False(t, status.NeedDelete)
		})
	}
}
