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
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// mockLogger is a simple logger implementation for testing
type mockLogger struct{}

func (m mockLogger) Init(info logr.RuntimeInfo)                                {}
func (m mockLogger) Info(level int, msg string, keysAndValues ...interface{})  {}
func (m mockLogger) Error(err error, msg string, keysAndValues ...interface{}) {}
func (m mockLogger) Enabled(level int) bool                                    { return false }
func (m mockLogger) WithValues(keysAndValues ...interface{}) logr.LogSink      { return m }
func (m mockLogger) WithName(name string) logr.LogSink                         { return m }

var testLogger = logr.New(mockLogger{})

func Test_scheduleSingleTaskNode(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()
	mockTimeNow := time.Now()
	o := timeNow
	timeNow = func() time.Time {
		return mockTimeNow
	}
	defer func() {
		timeNow = o
	}()
	type args struct {
		tNode             *taskNode
		taskClientCreator func(endpoint string) taskClient
	}
	tests := []struct {
		name           string
		args           args
		expectTaskNode *taskNode
	}{
		{
			name: "pending task node, deleting ",
			args: args{
				tNode: &taskNode{
					ObjectMeta: v1.ObjectMeta{
						Name:              "test-batch-sandbox-0",
						DeletionTimestamp: &metav1.Time{Time: mockTimeNow},
					},
				},
			},
			expectTaskNode: &taskNode{
				ObjectMeta: v1.ObjectMeta{
					Name:              "test-batch-sandbox-0",
					DeletionTimestamp: &metav1.Time{Time: mockTimeNow},
				},
				sState:              stateReleased,
				sStateLastTransTime: &mockTimeNow,
			},
		},
		{
			name: "assigned task node, task state=Running, deleting; setTask(nil)",
			args: args{
				tNode: &taskNode{
					ObjectMeta: v1.ObjectMeta{
						Name:              "test-batch-sandbox-0",
						DeletionTimestamp: &metav1.Time{Time: mockTimeNow},
					},
					IP: "1.2.3.4",
					Status: &api.Task{
						ProcessStatus: &api.ProcessStatus{
							Running: &api.Running{
								StartedAt: metav1.NewTime(mockTimeNow),
							},
						},
					},
					tState: RunningTaskState,
				},
				taskClientCreator: func(endpoint string) taskClient {
					mock := NewMocktaskClient(ctl)
					mock.EXPECT().Set(gomock.Any(), nil).Return(nil, nil).Times(1)
					return mock
				},
			},
			expectTaskNode: &taskNode{
				ObjectMeta: v1.ObjectMeta{
					Name:              "test-batch-sandbox-0",
					DeletionTimestamp: &metav1.Time{Time: mockTimeNow},
				},
				IP: "1.2.3.4",
				Status: &api.Task{
					ProcessStatus: &api.ProcessStatus{
						Running: &api.Running{
							StartedAt: metav1.NewTime(mockTimeNow),
						},
					},
				},
				tState:              RunningTaskState,
				sState:              stateReleasing,
				sStateLastTransTime: &mockTimeNow,
			},
		},
		{
			name: "assigned task node, task state=Running; setTask(task)",
			args: args{
				tNode: &taskNode{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-batch-sandbox-0",
					},
					IP: "1.2.3.4",
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"hello"},
						},
					},
					Status: &api.Task{
						ProcessStatus: &api.ProcessStatus{
							Running: &api.Running{
								StartedAt: metav1.NewTime(mockTimeNow),
							},
						},
					},
					tState: RunningTaskState,
				},
				taskClientCreator: func(endpoint string) taskClient {
					mock := NewMocktaskClient(ctl)
					mock.EXPECT().Set(gomock.Any(), &api.Task{
						Name: "test-batch-sandbox-0",
						Process: &api.Process{
							Command: []string{"hello"},
						},
					}).Return(nil, nil).Times(1)
					return mock
				},
			},
			expectTaskNode: &taskNode{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-batch-sandbox-0",
				},
				IP: "1.2.3.4",
				Spec: taskSpec{
					Process: &api.Process{
						Command: []string{"hello"},
					},
				},
				Status: &api.Task{
					ProcessStatus: &api.ProcessStatus{
						Running: &api.Running{
							StartedAt: metav1.NewTime(mockTimeNow),
						},
					},
				},
				tState: RunningTaskState,
			},
		},
		{
			name: "assigned task node, task state=Succeed, endpoint return nil task; sState trans from releasing -> released ",
			args: args{
				tNode: &taskNode{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-batch-sandbox-0",
					},
					IP: "1.2.3.4",
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"hello"},
						},
					},
					Status: nil,
					tState: SucceedTaskState,
					sState: stateReleasing,
				},
			},
			expectTaskNode: &taskNode{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-batch-sandbox-0",
				},
				IP: "1.2.3.4",
				Spec: taskSpec{
					Process: &api.Process{
						Command: []string{"hello"},
					},
				},
				Status:              nil,
				tState:              SucceedTaskState,
				sState:              stateReleased,
				sStateLastTransTime: &mockTimeNow,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduleSingleTaskNode(tt.args.tNode, tt.args.taskClientCreator, "", testLogger)
			if !reflect.DeepEqual(tt.expectTaskNode, tt.args.tNode) {
				t.Errorf("scheduleSingleTaskNode, want %+v, got %+v", tt.expectTaskNode, tt.args.tNode)
			}
		})
	}
}

func Test_assignTaskNodes(t *testing.T) {
	type args struct {
		taskNodes []*taskNode
		freePods  []*corev1.Pod
	}
	tests := []struct {
		name            string
		args            args
		want            []*corev1.Pod
		expectTaskNodes []*taskNode
	}{
		{
			name: "empty free pods, no assignment",
			args: args{
				taskNodes: []*taskNode{
					{
						ObjectMeta: v1.ObjectMeta{Name: "test-0"},
					},
				},
			},
			expectTaskNodes: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{Name: "test-0"},
				},
			},
		},
		{
			name: "free pods, assign",
			args: args{
				taskNodes: []*taskNode{
					{
						ObjectMeta: v1.ObjectMeta{Name: "test-0"},
					},
				},
				freePods: []*corev1.Pod{
					{
						ObjectMeta: v1.ObjectMeta{Name: "pod-hello-world"},
						Status:     corev1.PodStatus{PodIP: "1.2.3.4"},
					},
				},
			},
			want: []*corev1.Pod{},
			expectTaskNodes: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{Name: "test-0"},
					IP:         "1.2.3.4",
					PodName:    "pod-hello-world",
				},
			},
		},
		{
			name: "free pods, no unassigned task nodes, no assignment",
			args: args{
				taskNodes: []*taskNode{
					{
						ObjectMeta: v1.ObjectMeta{Name: "test-0"},
						IP:         "4.3.2.1",
						PodName:    "pod-foo-bar",
					},
				},
				freePods: []*corev1.Pod{
					{
						ObjectMeta: v1.ObjectMeta{Name: "pod-hello-world"},
						Status:     corev1.PodStatus{PodIP: "1.2.3.4"},
					},
				},
			},
			want: []*corev1.Pod{
				{
					ObjectMeta: v1.ObjectMeta{Name: "pod-hello-world"},
					Status:     corev1.PodStatus{PodIP: "1.2.3.4"},
				},
			},
			expectTaskNodes: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{Name: "test-0"},
					IP:         "4.3.2.1",
					PodName:    "pod-foo-bar",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assignTaskNodes(tt.args.taskNodes, tt.args.freePods, testLogger); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("assignTaskNodes() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.expectTaskNodes, tt.args.taskNodes) {
				t.Errorf("assignTaskNodes() = %v, want %v", tt.expectTaskNodes, tt.args.taskNodes)
			}
		})
	}
}

func Test_refreshFreePods(t *testing.T) {
	tests := []struct {
		name          string
		allPods       []*corev1.Pod
		taskNodes     []*taskNode
		expectedFree  int
		expectedNames []string
	}{
		{
			name: "no assigned pods",
			allPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
			},
			taskNodes: []*taskNode{
				{ObjectMeta: metav1.ObjectMeta{Name: "task-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "task-2"}},
			},
			expectedFree:  2,
			expectedNames: []string{"pod-1", "pod-2"},
		},
		{
			name: "some assigned pods",
			allPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-3"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.3"},
				},
			},
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "task-2"}},
			},
			expectedFree:  2,
			expectedNames: []string{"pod-2", "pod-3"},
		},
		{
			name: "all pods assigned",
			allPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
			},
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					IP:         "1.1.1.2",
					PodName:    "pod-2",
				},
			},
			expectedFree:  0,
			expectedNames: []string{},
		},
		{
			name: "pods without IP addresses",
			allPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: ""},
				},
			},
			taskNodes: []*taskNode{
				{ObjectMeta: metav1.ObjectMeta{Name: "task-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "task-2"}},
			},
			expectedFree:  1,
			expectedNames: []string{"pod-1"},
		},
		{
			name:    "empty pods list",
			allPods: []*corev1.Pod{},
			taskNodes: []*taskNode{
				{ObjectMeta: metav1.ObjectMeta{Name: "task-1"}},
			},
			expectedFree:  0,
			expectedNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := &defaultTaskScheduler{
				allPods:   tt.allPods,
				taskNodes: tt.taskNodes,
			}

			sch.refreshFreePods()

			if len(sch.freePods) != tt.expectedFree {
				t.Errorf("refreshFreePods() freePods length = %v, want %v", len(sch.freePods), tt.expectedFree)
			}

			actualNames := make([]string, len(sch.freePods))
			for i, pod := range sch.freePods {
				actualNames[i] = pod.Name
			}

			if !reflect.DeepEqual(actualNames, tt.expectedNames) {
				t.Errorf("refreshFreePods() freePods names = %v, want %v", actualNames, tt.expectedNames)
			}
		})
	}
}

func Test_collectTaskStatus(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockTimeNow := time.Now()
	o := timeNow
	timeNow = func() time.Time {
		return mockTimeNow
	}
	defer func() {
		timeNow = o
	}()

	tests := []struct {
		name               string
		taskNodes          []*taskNode
		expectedCollectIPs []string
		mockReturnTasks    map[string]*api.Task
		expectedTaskNodes  []*taskNode
	}{
		{
			name: "no assigned task nodes",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
			},
			expectedCollectIPs: []string{},
			mockReturnTasks:    map[string]*api.Task{},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
			},
		},
		{
			name: "assigned task nodes with task status",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					IP:         "1.1.1.2",
					PodName:    "pod-2",
				},
			},
			expectedCollectIPs: []string{"1.1.1.1", "1.1.1.2"},
			mockReturnTasks: map[string]*api.Task{
				"1.1.1.1": {
					Name: "task-1",
					ProcessStatus: &api.ProcessStatus{
						Running: &api.Running{
							StartedAt: metav1.NewTime(mockTimeNow),
						},
					},
				},
				"1.1.1.2": {
					Name: "task-2",
					ProcessStatus: &api.ProcessStatus{
						Terminated: &api.Terminated{
							ExitCode:   0,
							FinishedAt: metav1.NewTime(mockTimeNow),
						},
					},
				},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
					Status: &api.Task{
						Name: "task-1",
						ProcessStatus: &api.ProcessStatus{
							Running: &api.Running{
								StartedAt: metav1.NewTime(mockTimeNow),
							},
						},
					},
					tState:              RunningTaskState,
					tStateLastTransTime: &mockTimeNow,
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					IP:         "1.1.1.2",
					PodName:    "pod-2",
					Status: &api.Task{
						Name: "task-2",
						ProcessStatus: &api.ProcessStatus{
							Terminated: &api.Terminated{
								ExitCode:   0,
								FinishedAt: metav1.NewTime(mockTimeNow),
							},
						},
					},
					tState:              SucceedTaskState,
					tStateLastTransTime: &mockTimeNow,
				},
			},
		},
		{
			name: "assigned task nodes with nil task status",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
			},
			expectedCollectIPs: []string{"1.1.1.1"},
			mockReturnTasks: map[string]*api.Task{
				"1.1.1.1": nil,
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
			},
		},
		{
			name: "mixed assigned and unassigned task nodes",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
			},
			expectedCollectIPs: []string{"1.1.1.1"},
			mockReturnTasks: map[string]*api.Task{
				"1.1.1.1": {
					Name: "task-1",
					ProcessStatus: &api.ProcessStatus{
						Running: &api.Running{
							StartedAt: metav1.NewTime(mockTimeNow),
						},
					},
				},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					IP:         "1.1.1.1",
					PodName:    "pod-1",
					Status: &api.Task{
						Name: "task-1",
						ProcessStatus: &api.ProcessStatus{
							Running: &api.Running{
								StartedAt: metav1.NewTime(mockTimeNow),
							},
						},
					},
					tState:              RunningTaskState,
					tStateLastTransTime: &mockTimeNow,
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock task status collector
			mockCollector := NewMocktaskStatusCollector(ctl)
			if len(tt.expectedCollectIPs) > 0 {
				mockCollector.EXPECT().Collect(gomock.Any(), tt.expectedCollectIPs).Return(tt.mockReturnTasks).Times(1)
			}

			// Create scheduler with mock collector
			sch := &defaultTaskScheduler{
				taskNodes:           tt.taskNodes,
				taskStatusCollector: mockCollector,
				logger:              testLogger,
			}

			// Call collectTaskStatus
			sch.collectTaskStatus(tt.taskNodes)

			// Verify results
			for i, expectedNode := range tt.expectedTaskNodes {
				actualNode := tt.taskNodes[i]

				if actualNode.Name != expectedNode.Name {
					t.Errorf("taskNode[%d].Name = %v, want %v", i, actualNode.Name, expectedNode.Name)
				}

				if actualNode.IP != expectedNode.IP {
					t.Errorf("taskNode[%d].IP = %v, want %v", i, actualNode.IP, expectedNode.IP)
				}

				if actualNode.PodName != expectedNode.PodName {
					t.Errorf("taskNode[%d].PodName = %v, want %v", i, actualNode.PodName, expectedNode.PodName)
				}

				if expectedNode.Status == nil {
					if actualNode.Status != nil {
						t.Errorf("taskNode[%d].Status = %v, want nil", i, actualNode.Status)
					}
				} else {
					if actualNode.Status == nil {
						t.Errorf("taskNode[%d].Status = nil, want %v", i, expectedNode.Status)
					} else if actualNode.Status.Name != expectedNode.Status.Name {
						t.Errorf("taskNode[%d].Status.Name = %v, want %v", i, actualNode.Status.Name, expectedNode.Status.Name)
					}
				}

				if actualNode.tState != expectedNode.tState {
					t.Errorf("taskNode[%d].tState = %v, want %v", i, actualNode.tState, expectedNode.tState)
				}

				// Compare time pointers
				if expectedNode.tStateLastTransTime == nil {
					if actualNode.tStateLastTransTime != nil {
						t.Errorf("taskNode[%d].tStateLastTransTime = %v, want nil", i, actualNode.tStateLastTransTime)
					}
				} else {
					if actualNode.tStateLastTransTime == nil {
						t.Errorf("taskNode[%d].tStateLastTransTime = nil, want %v", i, expectedNode.tStateLastTransTime)
					} else if !actualNode.tStateLastTransTime.Equal(*expectedNode.tStateLastTransTime) {
						t.Errorf("taskNode[%d].tStateLastTransTime = %v, want %v", i, actualNode.tStateLastTransTime, expectedNode.tStateLastTransTime)
					}
				}
			}
		})
	}
}

func Test_indexByName(t *testing.T) {
	tests := []struct {
		name      string
		taskNodes []*taskNode
		expected  map[string]*taskNode
	}{
		{
			name:      "empty task nodes",
			taskNodes: []*taskNode{},
			expected:  map[string]*taskNode{},
		},
		{
			name: "single task node",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
			},
			expected: map[string]*taskNode{
				"task-1": {
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
			},
		},
		{
			name: "multiple task nodes",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-3"},
				},
			},
			expected: map[string]*taskNode{
				"task-1": {
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
				"task-2": {
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
				},
				"task-3": {
					ObjectMeta: metav1.ObjectMeta{Name: "task-3"},
				},
			},
		},
		{
			name: "duplicate task node names",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
			},
			expected: map[string]*taskNode{
				"task-1": {
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := indexByName(tt.taskNodes)

			if len(result) != len(tt.expected) {
				t.Errorf("indexByName() map length = %v, want %v", len(result), len(tt.expected))
			}

			for key, expectedNode := range tt.expected {
				actualNode, ok := result[key]
				if !ok {
					t.Errorf("indexByName() missing key %v", key)
					continue
				}

				if actualNode.Name != expectedNode.Name {
					t.Errorf("indexByName()[%v].Name = %v, want %v", key, actualNode.Name, expectedNode.Name)
				}
			}
		})
	}
}

func Test_scheduleTaskNodes(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	// Mock time for consistent testing
	mockTimeNow := time.Now()
	o := timeNow
	timeNow = func() time.Time {
		return mockTimeNow
	}
	defer func() {
		timeNow = o
	}()

	tests := []struct {
		name                      string
		taskNodes                 []*taskNode
		freePods                  []*corev1.Pod
		batchSbx                  *sandboxv1alpha1.BatchSandbox
		expectedTaskNodes         []*taskNode
		expectedRemainingFreePods int
		expectedSetCalls          map[string]*api.Task // IP -> Expected Task
	}{
		{
			name: "assign free pods to unassigned task nodes",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			freePods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
			},
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: v1.ObjectMeta{Name: "test-batch"},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					IP:      "1.1.1.1",
					PodName: "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
					IP:      "1.1.1.2",
					PodName: "pod-2",
				},
			},
			expectedRemainingFreePods: 0,
			expectedSetCalls: map[string]*api.Task{
				"1.1.1.1": {
					Name: "task-1",
					Process: &api.Process{
						Command: []string{"echo", "hello"},
					},
				},
				"1.1.1.2": {
					Name: "task-2",
					Process: &api.Process{
						Command: []string{"echo", "world"},
					},
				},
			},
		},
		{
			name: "no free pods available",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			freePods: []*corev1.Pod{},
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: v1.ObjectMeta{Name: "test-batch"},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			expectedRemainingFreePods: 0,
			expectedSetCalls:          map[string]*api.Task{},
		},
		{
			name: "some task nodes already assigned",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					IP:      "1.1.1.1",
					PodName: "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			freePods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
			},
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: v1.ObjectMeta{Name: "test-batch"},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					IP:      "1.1.1.1",
					PodName: "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
					IP:      "1.1.1.2",
					PodName: "pod-2",
				},
			},
			expectedRemainingFreePods: 0,
			expectedSetCalls: map[string]*api.Task{
				"1.1.1.1": {
					Name: "task-1",
					Process: &api.Process{
						Command: []string{"echo", "hello"},
					},
				},
				"1.1.1.2": {
					Name: "task-2",
					Process: &api.Process{
						Command: []string{"echo", "world"},
					},
				},
			},
		},
		{
			name: "more free pods than unassigned tasks",
			taskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					IP:      "1.1.1.1",
					PodName: "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			freePods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod-3"},
					Status:     corev1.PodStatus{PodIP: "1.1.1.3"},
				},
			},
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: v1.ObjectMeta{Name: "test-batch"},
			},
			expectedTaskNodes: []*taskNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					IP:      "1.1.1.1",
					PodName: "pod-1",
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2"},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
					IP:      "1.1.1.2",
					PodName: "pod-2",
				},
			},
			expectedRemainingFreePods: 1,
			expectedSetCalls: map[string]*api.Task{
				"1.1.1.1": {
					Name: "task-1",
					Process: &api.Process{
						Command: []string{"echo", "hello"},
					},
				},
				"1.1.1.2": {
					Name: "task-2",
					Process: &api.Process{
						Command: []string{"echo", "world"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock task clients for each pod IP and task node
			mockClients := make(map[string]*MocktaskClient)

			// Create task client creator function that returns mock clients
			taskClientCreator := func(ip string) taskClient {
				if mockClient, ok := mockClients[ip]; ok {
					return mockClient
				}
				mockClient := NewMocktaskClient(ctl)
				mockClients[ip] = mockClient
				return mockClient
			}

			// Set expectations for Set calls
			for ip, expectedTask := range tt.expectedSetCalls {
				mockClient := mockClients[ip]
				if mockClient == nil {
					mockClient = NewMocktaskClient(ctl)
					mockClients[ip] = mockClient
				}
				mockClient.EXPECT().Set(gomock.Any(), expectedTask).Return(expectedTask, nil).Times(1)
			}

			// Create scheduler
			sch := &defaultTaskScheduler{
				taskNodes:         tt.taskNodes,
				freePods:          tt.freePods,
				maxConcurrency:    defaultSchConcurrency,
				taskClientCreator: taskClientCreator,
				logger:            testLogger,
			}

			// Call scheduleTaskNodes
			err := sch.scheduleTaskNodes()

			// Verify no error
			if err != nil {
				t.Errorf("scheduleTaskNodes() error = %v, want nil", err)
			}

			// Verify results
			for i, expectedNode := range tt.expectedTaskNodes {
				actualNode := tt.taskNodes[i]

				if actualNode.Name != expectedNode.Name {
					t.Errorf("taskNode[%d].Name = %v, want %v", i, actualNode.Name, expectedNode.Name)
				}

				if actualNode.IP != expectedNode.IP {
					t.Errorf("taskNode[%d].IP = %v, want %v", i, actualNode.IP, expectedNode.IP)
				}

				if actualNode.PodName != expectedNode.PodName {
					t.Errorf("taskNode[%d].PodName = %v, want %v", i, actualNode.PodName, expectedNode.PodName)
				}
			}

			// Verify remaining free pods
			if len(sch.freePods) != tt.expectedRemainingFreePods {
				t.Errorf("scheduleTaskNodes() remaining freePods length = %v, want %v", len(sch.freePods), tt.expectedRemainingFreePods)
			}
		})
	}
}

func Test_parseTaskState(t *testing.T) {
	mockTimeNow := time.Now()

	tests := []struct {
		name     string
		task     *api.Task
		expected TaskState
	}{
		{
			name: "running task",
			task: &api.Task{
				ProcessStatus: &api.ProcessStatus{
					Running: &api.Running{
						StartedAt: metav1.NewTime(mockTimeNow),
					},
				},
			},
			expected: RunningTaskState,
		},
		{
			name: "succeed task",
			task: &api.Task{
				ProcessStatus: &api.ProcessStatus{
					Terminated: &api.Terminated{
						ExitCode:   0,
						FinishedAt: metav1.NewTime(mockTimeNow),
					},
				},
			},
			expected: SucceedTaskState,
		},
		{
			name: "failed task",
			task: &api.Task{
				ProcessStatus: &api.ProcessStatus{
					Terminated: &api.Terminated{
						ExitCode:   1,
						FinishedAt: metav1.NewTime(mockTimeNow),
					},
				},
			},
			expected: FailedTaskState,
		},
		{
			name: "unknown task state",
			task: &api.Task{
				ProcessStatus: &api.ProcessStatus{},
			},
			expected: UnknownTaskState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTaskState(tt.task)
			if result != tt.expected {
				t.Errorf("parseTaskState() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func Test_initTaskNodes(t *testing.T) {
	type args struct {
		tasks []*api.Task
	}
	tests := []struct {
		name    string
		args    args
		want    []*taskNode
		wantErr bool
	}{
		{
			name: "init success",
			args: args{
				tasks: []*api.Task{
					{
						Name: "test-task-0",
						Process: &api.Process{
							Command: []string{"tail", "-f", "/dev/null"},
						},
					},
				},
			},
			want: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-task-0",
					},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"tail", "-f", "/dev/null"},
						}},
				},
			},
		},
		{
			name: "init multiple tasks",
			args: args{
				tasks: []*api.Task{
					{
						Name: "test-task-0",
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						},
					},
					{
						Name: "test-task-1",
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
			want: []*taskNode{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-task-0",
					},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "hello"},
						}},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-task-1",
					},
					Spec: taskSpec{
						Process: &api.Process{
							Command: []string{"echo", "world"},
						},
					},
				},
			},
		},
		{
			name: "init empty tasks",
			args: args{
				tasks: []*api.Task{},
			},
			want: []*taskNode{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := initTaskNodes(tt.args.tasks)
			if (err != nil) != tt.wantErr {
				t.Errorf("initTaskNodes() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("initTaskNodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_addTasks(t *testing.T) {
	tests := []struct {
		name          string
		initial       []*api.Task
		addTasks      []*api.Task
		wantNodeNames []string
		wantNodeCount int
	}{
		{
			name: "scale-out: new tasks are appended, existing tasks are skipped",
			initial: []*api.Task{
				{Name: "sandbox-0", Process: &api.Process{Command: []string{"echo", "0"}}},
			},
			addTasks: []*api.Task{
				{Name: "sandbox-0", Process: &api.Process{Command: []string{"echo", "0"}}},
				{Name: "sandbox-1", Process: &api.Process{Command: []string{"echo", "1"}}},
			},
			wantNodeNames: []string{"sandbox-0", "sandbox-1"},
			wantNodeCount: 2,
		},
		{
			name: "no-op: add same tasks as already tracked",
			initial: []*api.Task{
				{Name: "sandbox-0"},
				{Name: "sandbox-1"},
			},
			addTasks: []*api.Task{
				{Name: "sandbox-0"},
				{Name: "sandbox-1"},
			},
			wantNodeNames: []string{"sandbox-0", "sandbox-1"},
			wantNodeCount: 2,
		},
		{
			name:    "empty scheduler: add initial tasks",
			initial: []*api.Task{},
			addTasks: []*api.Task{
				{Name: "sandbox-0"},
			},
			wantNodeNames: []string{"sandbox-0"},
			wantNodeCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initialNodes, err := initTaskNodes(tt.initial)
			if err != nil {
				t.Fatalf("initTaskNodes() error = %v", err)
			}
			sch := &defaultTaskScheduler{
				taskNodes:           initialNodes,
				taskNodeByNameIndex: indexByName(initialNodes),
				logger:              testLogger,
			}

			if err := sch.AddTasks(tt.addTasks); err != nil {
				t.Fatalf("AddTasks() unexpected error = %v", err)
			}

			if len(sch.taskNodes) != tt.wantNodeCount {
				t.Errorf("AddTasks() taskNodes count = %d, want %d", len(sch.taskNodes), tt.wantNodeCount)
			}

			nodeNames := make([]string, len(sch.taskNodes))
			for i, n := range sch.taskNodes {
				nodeNames[i] = n.Name
			}
			if !reflect.DeepEqual(nodeNames, tt.wantNodeNames) {
				t.Errorf("AddTasks() taskNode names = %v, want %v", nodeNames, tt.wantNodeNames)
			}

			if len(sch.taskNodeByNameIndex) != tt.wantNodeCount {
				t.Errorf("AddTasks() taskNodeByNameIndex size = %d, want %d", len(sch.taskNodeByNameIndex), tt.wantNodeCount)
			}
		})
	}
}
