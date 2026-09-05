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

package scheduler

import (
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/golang/mock/gomock"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func Test_recoverOneTaskNode(t *testing.T) {
	mockTimeNow := time.Now()
	o := timeNow
	timeNow = func() time.Time {
		return mockTimeNow
	}
	defer func() {
		timeNow = o
	}()
	testNow := metav1.Time{Time: mockTimeNow}
	testTask := &api.Task{
		Name: "test",
		Process: &api.Process{
			Command: []string{"sleep"},
		},
		ProcessStatus: &api.ProcessStatus{
			Running: &api.Running{
				StartedAt: testNow,
			},
		},
	}
	testReleasingTask := &api.Task{
		Name:              "test",
		DeletionTimestamp: &testNow,
		Process: &api.Process{
			Command: []string{"sleep"},
		},
		ProcessStatus: &api.ProcessStatus{
			Running: &api.Running{
				StartedAt: testNow,
			},
		},
	}
	type args struct {
		tNode       *taskNode
		currentTask *api.Task
		ip          string
		podName     string
	}
	tests := []struct {
		name           string
		args           args
		expectTaskNode *taskNode
	}{
		{
			name: "running task",
			args: args{
				tNode:       &taskNode{},
				currentTask: testTask,
				ip:          "1.2.3.4",
				podName:     "foo-bar",
			},
			expectTaskNode: &taskNode{
				Status:              testTask,
				IP:                  "1.2.3.4",
				PodName:             "foo-bar",
				tState:              RunningTaskState,
				tStateLastTransTime: &mockTimeNow,
			},
		},
		{
			name: "releasing task",
			args: args{
				tNode:       &taskNode{},
				currentTask: testReleasingTask,
				ip:          "1.2.3.4",
				podName:     "foo-bar",
			},
			expectTaskNode: &taskNode{
				Status:              testReleasingTask,
				IP:                  "1.2.3.4",
				PodName:             "foo-bar",
				sState:              stateReleasing,
				sStateLastTransTime: &mockTimeNow,
				tState:              RunningTaskState,
				tStateLastTransTime: &mockTimeNow,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recoverOneTaskNode(tt.args.tNode, tt.args.currentTask, tt.args.ip, tt.args.podName, testLogger)
			if tt.expectTaskNode != nil {
				if !reflect.DeepEqual(tt.expectTaskNode, tt.args.tNode) {
					t.Errorf("recoverOneTaskNode, want %+v, got %+v", tt.expectTaskNode, tt.args.tNode)
				}
			}
		})
	}
}

func Test_defaultTaskScheduler_recoverTaskNodesStatus(t *testing.T) {
	mockTimeNow := time.Now()
	o := timeNow
	timeNow = func() time.Time {
		return mockTimeNow
	}
	defer func() {
		timeNow = o
	}()
	ctl := gomock.NewController(t)
	defer ctl.Finish()
	testNow := metav1.Now()
	testTaskNode := &taskNode{
		ObjectMeta: v1.ObjectMeta{
			Name: "bsbx-0",
		},
		Spec: taskSpec{
			Process: &api.Process{
				Command: []string{"hello"},
			},
		},
	}
	testTask := &api.Task{
		Name:    testTaskNode.Name,
		Process: testTaskNode.Spec.Process,
		ProcessStatus: &api.ProcessStatus{
			Running: &api.Running{
				StartedAt: testNow,
			},
		},
	}
	recoveredTestTaskNode := &taskNode{
		ObjectMeta: v1.ObjectMeta{
			Name: "bsbx-0",
		},
		Spec: taskSpec{
			Process: &api.Process{
				Command: []string{"hello"},
			},
		},
		Status:              testTask,
		PodName:             "test-0",
		IP:                  "1.2.3.4",
		tState:              RunningTaskState,
		tStateLastTransTime: &mockTimeNow,
	}

	type fields struct {
		freePods            []*corev1.Pod
		allPods             []*corev1.Pod
		taskNodes           []*taskNode
		taskNodeByNameIndex map[string]*taskNode
		maxConcurrency      int
		taskStatusCollector taskStatusCollector
	}
	tests := []struct {
		name            string
		fields          fields
		wantErr         bool
		expectTaskNodes []*taskNode
	}{
		{
			name: "recover nothing, pod pending",
			fields: fields{
				allPods: []*corev1.Pod{{
					ObjectMeta: v1.ObjectMeta{Name: "test-0"},
				}},
				taskNodes: []*taskNode{
					{
						ObjectMeta: v1.ObjectMeta{
							Name: "bsbx-0",
						},
					},
				},
			},
			expectTaskNodes: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "bsbx-0",
					},
				},
			},
		},
		{
			name: "recover nothing, client return nil task via endpoint",
			fields: fields{
				allPods: []*corev1.Pod{{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-0",
					},
					Status: corev1.PodStatus{
						PodIP: "1.2.3.4",
					},
				}},
				taskNodes: []*taskNode{
					{
						ObjectMeta: v1.ObjectMeta{
							Name: "bsbx-0",
						},
					},
				},
				taskStatusCollector: func() taskStatusCollector {
					mock := NewMocktaskStatusCollector(ctl)
					mock.EXPECT().Collect(gomock.Any(), []string{"1.2.3.4"}).Return(map[string]*api.Task{"1.2.3.4": nil}, nil).Times(1)
					return mock
				}(),
			},
			expectTaskNodes: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "bsbx-0",
					},
				},
			},
		},
		{
			name: "recover successfully, client return running task via endpoint",
			fields: fields{
				allPods: []*corev1.Pod{{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-0",
					},
					Status: corev1.PodStatus{
						PodIP: "1.2.3.4",
					},
				}},
				taskNodes: []*taskNode{testTaskNode},
				taskNodeByNameIndex: map[string]*taskNode{
					"bsbx-0": testTaskNode,
				},
				taskStatusCollector: func() taskStatusCollector {
					mock := NewMocktaskStatusCollector(ctl)
					mock.EXPECT().Collect(gomock.Any(), []string{"1.2.3.4"}).Return(map[string]*api.Task{"1.2.3.4": testTask}, nil).Times(1)
					return mock
				}(),
			},
			expectTaskNodes: []*taskNode{
				recoveredTestTaskNode,
			},
		},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			sch := &defaultTaskScheduler{
				freePods:            tt.fields.freePods,
				allPods:             tt.fields.allPods,
				taskNodes:           tt.fields.taskNodes,
				taskNodeByNameIndex: tt.fields.taskNodeByNameIndex,
				maxConcurrency:      tt.fields.maxConcurrency,
				taskStatusCollector: tt.fields.taskStatusCollector,
				logger:              testLogger,
			}
			if err := sch.recoverTaskNodesStatus(); (err != nil) != tt.wantErr {
				t.Errorf("defaultTaskScheduler.recoverTaskNodesStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.expectTaskNodes != nil {
				if !reflect.DeepEqual(tt.expectTaskNodes, sch.taskNodes) {
					t.Errorf("recoverTaskNodesStatus, want %+v, got %+v", tt.expectTaskNodes, sch.taskNodes)
				}
			}
		})
	}
}

func Test_defaultTaskScheduler_recoverTaskNodesStatusIsAtomicOnCollectionError(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	firstNode := &taskNode{
		ObjectMeta: v1.ObjectMeta{Name: "task-1"},
		Status:     &api.Task{Name: "task-1"},
		IP:         "192.0.2.1",
		PodName:    "pod-1",
		tState:     RunningTaskState,
	}
	secondNode := &taskNode{
		ObjectMeta: v1.ObjectMeta{Name: "task-2"},
		Status:     &api.Task{Name: "task-2"},
		IP:         "192.0.2.2",
		PodName:    "pod-2",
		tState:     RunningTaskState,
	}
	firstNodeBefore := *firstNode
	secondNodeBefore := *secondNode
	queryErr := errors.New("executor unavailable")

	collector := NewMocktaskStatusCollector(ctl)
	collector.EXPECT().Collect(gomock.Any(), []string{"192.0.2.1", "192.0.2.2"}).Return(map[string]*api.Task{
		"192.0.2.1": {Name: "task-1"},
	}, queryErr).Times(1)

	sch := &defaultTaskScheduler{
		allPods: []*corev1.Pod{
			{ObjectMeta: v1.ObjectMeta{Name: "pod-1"}, Status: corev1.PodStatus{PodIP: "192.0.2.1"}},
			{ObjectMeta: v1.ObjectMeta{Name: "pod-2"}, Status: corev1.PodStatus{PodIP: "192.0.2.2"}},
		},
		taskNodes:           []*taskNode{firstNode, secondNode},
		taskNodeByNameIndex: map[string]*taskNode{"task-1": firstNode, "task-2": secondNode},
		taskStatusCollector: collector,
		logger:              testLogger,
	}

	err := sch.recoverTaskNodesStatus()
	if !errors.Is(err, queryErr) {
		t.Fatalf("recoverTaskNodesStatus() error = %v, want wrapped collection error", err)
	}
	if !reflect.DeepEqual(firstNodeBefore, *firstNode) {
		t.Fatalf("first task node changed after failed recovery: before=%+v after=%+v", firstNodeBefore, *firstNode)
	}
	if !reflect.DeepEqual(secondNodeBefore, *secondNode) {
		t.Fatalf("second task node changed after failed recovery: before=%+v after=%+v", secondNodeBefore, *secondNode)
	}
}

func Test_defaultTaskScheduler_recoverPropagatesCollectionError(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	queryErr := errors.New("executor unavailable")
	collector := NewMocktaskStatusCollector(ctl)
	collector.EXPECT().Collect(gomock.Any(), []string{"192.0.2.1"}).Return(nil, queryErr).Times(1)

	sch := &defaultTaskScheduler{
		allPods: []*corev1.Pod{{
			ObjectMeta: v1.ObjectMeta{Name: "pod-1"},
			Status:     corev1.PodStatus{PodIP: "192.0.2.1"},
		}},
		taskStatusCollector: collector,
		logger:              testLogger,
	}
	if err := sch.recover(); !errors.Is(err, queryErr) {
		t.Fatalf("recover() error = %v, want wrapped collection error", err)
	}
}

func Test_defaultTaskScheduler_recoverTaskNodesStatusRejectsIncompleteCollection(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	firstNode := &taskNode{ObjectMeta: v1.ObjectMeta{Name: "task-1"}}
	secondNode := &taskNode{ObjectMeta: v1.ObjectMeta{Name: "task-2"}}
	firstNodeBefore := *firstNode
	secondNodeBefore := *secondNode
	collector := NewMocktaskStatusCollector(ctl)
	collector.EXPECT().Collect(gomock.Any(), []string{"192.0.2.1", "192.0.2.2"}).Return(map[string]*api.Task{
		"192.0.2.1": {Name: "task-1"},
	}, nil).Times(1)

	sch := &defaultTaskScheduler{
		allPods: []*corev1.Pod{
			{ObjectMeta: v1.ObjectMeta{Name: "pod-1"}, Status: corev1.PodStatus{PodIP: "192.0.2.1"}},
			{ObjectMeta: v1.ObjectMeta{Name: "pod-2"}, Status: corev1.PodStatus{PodIP: "192.0.2.2"}},
		},
		taskNodes:           []*taskNode{firstNode, secondNode},
		taskNodeByNameIndex: map[string]*taskNode{"task-1": firstNode, "task-2": secondNode},
		taskStatusCollector: collector,
		logger:              testLogger,
	}

	if err := sch.recoverTaskNodesStatus(); err == nil {
		t.Fatal("recoverTaskNodesStatus() error = nil, want incomplete collection error")
	}
	if !reflect.DeepEqual(firstNodeBefore, *firstNode) {
		t.Fatalf("first task node changed after incomplete recovery: before=%+v after=%+v", firstNodeBefore, *firstNode)
	}
	if !reflect.DeepEqual(secondNodeBefore, *secondNode) {
		t.Fatalf("second task node changed after incomplete recovery: before=%+v after=%+v", secondNodeBefore, *secondNode)
	}
}
