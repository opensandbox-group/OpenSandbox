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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/recycle/restart"
)

func TestRestartRecycler(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		handlerStatus  *restart.Status
		handlerErr     error
		wantState      string
		wantNeedDelete bool
		wantErr        bool
		wantNilStatus  bool
	}{
		{
			name:           "NilPod_Succeeded",
			pod:            nil,
			wantState:      StateSucceeded,
			wantNeedDelete: false,
		},
		{
			name:          "HandlerSucceeded",
			pod:           &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			handlerStatus: &restart.Status{State: restart.StateSucceeded},
			wantState:     StateSucceeded,
		},
		{
			name:           "HandlerFailed_NeedDelete",
			pod:            &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			handlerStatus:  &restart.Status{State: restart.StateFailed},
			wantState:      StateFailed,
			wantNeedDelete: true,
		},
		{
			name:          "HandlerRecycling",
			pod:           &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			handlerStatus: &restart.Status{State: restart.StateRestarting},
			wantState:     StateRecycling,
		},
		{
			name:          "HandlerError",
			pod:           &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			handlerErr:    assert.AnError,
			wantErr:       true,
			wantNilStatus: true,
		},
	}
	pool := &sandboxv1alpha1.Pool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockHandler := restart.NewMockHandler(ctrl)
			if tt.pod != nil {
				mockHandler.EXPECT().
					TryRestart(gomock.Any(), pool, tt.pod, gomock.Any()).
					Return(tt.handlerStatus, tt.handlerErr)
			}
			r := NewRestartRecycler(mockHandler)
			status, err := r.TryRecycle(context.Background(), pool, tt.pod, &Spec{ID: "sbx1"})
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantState, status.State)
				assert.Equal(t, tt.wantNeedDelete, status.NeedDelete)
			}
			if tt.wantNilStatus {
				assert.Nil(t, status)
			}
		})
	}
}
