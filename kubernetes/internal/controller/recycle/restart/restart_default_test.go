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

package restart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// --- parseConfig tests ---

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name          string
		annotations   map[string]string
		wantBlacklist []string
		wantInterval  string
		wantRetries   int32
		wantCommand   []string
	}{
		{
			name:         "NilAnnotations",
			annotations:  nil,
			wantInterval: DefaultRetryInterval.String(),
			wantRetries:  DefaultMaxRetries,
			wantCommand:  DefaultRestartCommand,
		},
		{
			name:         "EmptyAnnotations",
			annotations:  map[string]string{},
			wantInterval: DefaultRetryInterval.String(),
			wantRetries:  DefaultMaxRetries,
			wantCommand:  DefaultRestartCommand,
		},
		{
			name: "ValidConfig",
			annotations: map[string]string{
				AnnoRestartConfigKey: `{"blacklist":["sidecar","init"],"retryInterval":"60s","maxRetries":5,"restartCommand":["/sbin/init"]}`,
			},
			wantBlacklist: []string{"sidecar", "init"},
			wantInterval:  "60s",
			wantRetries:   5,
			wantCommand:   []string{"/sbin/init"},
		},
		{
			name: "PartialConfig_FallbackToDefaults",
			annotations: map[string]string{
				AnnoRestartConfigKey: `{"blacklist":["sidecar"]}`,
			},
			wantBlacklist: []string{"sidecar"},
			wantInterval:  DefaultRetryInterval.String(),
			wantRetries:   DefaultMaxRetries,
			wantCommand:   DefaultRestartCommand,
		},
		{
			name: "InvalidJSON_FallbackToDefaults",
			annotations: map[string]string{
				AnnoRestartConfigKey: `{invalid json}`,
			},
			wantInterval: DefaultRetryInterval.String(),
			wantRetries:  DefaultMaxRetries,
			wantCommand:  DefaultRestartCommand,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := parseConfig(context.Background(), tt.annotations)
			assert.Equal(t, tt.wantBlacklist, cfg.Blacklist)
			assert.Equal(t, tt.wantInterval, cfg.RetryInterval)
			assert.Equal(t, tt.wantRetries, cfg.MaxRetries)
			assert.Equal(t, tt.wantCommand, cfg.RestartCommand)
		})
	}
}

// --- initInfo tests ---

func TestInitInfo(t *testing.T) {
	runningMain := corev1.ContainerStatus{
		Name: "main", ContainerID: "docker://abc123",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	runningSidecar := corev1.ContainerStatus{
		Name: "sidecar", ContainerID: "docker://def456",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	tests := []struct {
		name              string
		cfg               restartConfig
		pod               *corev1.Pod
		opts              *Spec
		wantID            string
		wantContainers    []string
		wantNotContainers []string
	}{
		{
			name: "NoOptions_AllRunning",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{runningMain, runningSidecar}},
			},
			opts:           nil,
			wantContainers: []string{"main", "sidecar"},
		},
		{
			name: "Blacklist_ExcludesSidecar",
			cfg:  restartConfig{Blacklist: []string{"sidecar"}},
			pod: &corev1.Pod{
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{runningMain, runningSidecar}},
			},
			opts:              &Spec{ID: "sbx1"},
			wantID:            "sbx1",
			wantContainers:    []string{"main"},
			wantNotContainers: []string{"sidecar"},
		},
		{
			name: "OnlyRunningContainers",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "running", ContainerID: "docker://abc123", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
						{Name: "waiting", ContainerID: "docker://def456", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}},
						{Name: "terminated", ContainerID: "docker://ghi789", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
					},
				},
			},
			opts:              nil,
			wantContainers:    []string{"running"},
			wantNotContainers: []string{"waiting", "terminated"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &defaultRestartHandler{}
			info := h.initInfo(tt.cfg, tt.pod, tt.opts)
			assert.Equal(t, tt.wantID, info.ID)
			for _, name := range tt.wantContainers {
				_, ok := info.Containers[name]
				assert.True(t, ok, "expected container %q in info", name)
			}
			for _, name := range tt.wantNotContainers {
				_, ok := info.Containers[name]
				assert.False(t, ok, "unexpected container %q in info", name)
			}
		})
	}
}

// --- evalState tests ---

func TestEvalState(t *testing.T) {
	tests := []struct {
		name      string
		info      *restartInfo
		pod       *corev1.Pod
		wantState State
	}{
		{
			name: "Succeeded_ContainerIDChanged",
			info: &restartInfo{Containers: map[string]string{"main": "docker://old-id"}},
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: "docker://new-id", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}}},
			wantState: StateSucceeded,
		},
		{
			name: "Restarting_SameContainerID",
			info: &restartInfo{Containers: map[string]string{"main": "docker://old-id"}},
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: "docker://old-id", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}}},
			wantState: StateRestarting,
		},
		{
			name: "Restarting_ContainerNotRunning",
			info: &restartInfo{Containers: map[string]string{"main": "docker://old-id"}},
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: "docker://new-id", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}},
			}}},
			wantState: StateRestarting,
		},
		{
			name:      "Restarting_ContainerNotFound",
			info:      &restartInfo{Containers: map[string]string{"main": "docker://old-id"}},
			pod:       &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{}}},
			wantState: StateRestarting,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &defaultRestartHandler{}
			assert.Equal(t, tt.wantState, h.evalState(tt.pod, tt.info))
		})
	}
}

// --- loadInfo tests ---

func TestLoadInfo(t *testing.T) {
	validInfo := &restartInfo{
		ID:         "sbx1",
		StartTime:  "2026-01-01T00:00:00Z",
		Containers: map[string]string{"main": "docker://abc123"},
	}
	validRaw, _ := json.Marshal(validInfo)
	tests := []struct {
		name       string
		pod        *corev1.Pod
		wantErr    bool
		wantID     string
		wantMainID string
	}{
		{
			name:    "NoAnnotation",
			pod:     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"}},
			wantErr: true,
		},
		{
			name: "ValidAnnotation",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "pod1", Namespace: "default",
				Annotations: map[string]string{AnnoRestartRecordKey: string(validRaw)},
			}},
			wantID:     "sbx1",
			wantMainID: "docker://abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &defaultRestartHandler{}
			loaded, err := h.loadInfo(tt.pod)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantID, loaded.ID)
			assert.Equal(t, tt.wantMainID, loaded.Containers["main"])
		})
	}
}

// --- RestartCommand tests ---

func TestDefaultRestartCommand_SendsSIGTERM(t *testing.T) {
	// DefaultRestartCommand sends SIGTERM to PID 1 via "kill 1".
	// Containers using the Restart recycle strategy must run a process that
	// responds to SIGTERM (e.g., a real server binary, not bare "sleep").
	assert.Equal(t, []string{"kill", "1"}, DefaultRestartCommand)
}

// --- TryRestart tests ---

// stubExec is a test-only containerExec implementation.
type stubExec struct {
	err    error
	called []string // container names that were exec'd
}

func (s *stubExec) exec(_ context.Context, _ *corev1.Pod, containerName string, _ []string) error {
	s.called = append(s.called, containerName)
	return s.err
}

// newTestHandler builds a defaultRestartHandler wired to a fake client and stubExec.
func newTestHandler(t *testing.T, objs ...runtime.Object) (*defaultRestartHandler, *stubExec) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	stub := &stubExec{}
	return &defaultRestartHandler{
		client:      fakeClient,
		execTimeout: DefaultExecTimeout,
		exec:        stub,
	}, stub
}

func TestTryRestart(t *testing.T) {
	const sbxID = "sbx1"
	pool := &sandboxv1alpha1.Pool{}
	opts := &Spec{ID: sbxID}

	newRunningPod := func(name string, containerIDs ...string) *corev1.Pod {
		statuses := make([]corev1.ContainerStatus, len(containerIDs))
		for i, cid := range containerIDs {
			statuses[i] = corev1.ContainerStatus{
				Name:        fmt.Sprintf("c%d", i),
				ContainerID: cid,
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
		}
	}

	// podWithRecord returns a pod that already has a restart record annotation.
	podWithRecord := func(base *corev1.Pod, record *restartInfo) *corev1.Pod {
		raw, _ := json.Marshal(record)
		pod := base.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnoRestartRecordKey] = string(raw)
		return pod
	}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		execErr   error
		wantState State
		wantErr   bool
		wantExecd int // expected number of exec calls
	}{
		{
			name:      "FirstCall_InitializesAndExecs",
			pod:       newRunningPod("pod1", "docker://old"),
			wantState: StateRestarting,
			wantExecd: 1,
		},
		{
			name:      "PodNotRunning_ReturnsError",
			pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
			wantErr:   true,
			wantExecd: 0,
		},
		{
			name: "AlreadySucceeded_SkipsExec",
			pod: podWithRecord(
				newRunningPod("pod1", "docker://new"),
				&restartInfo{ID: sbxID, StartTime: "2026-01-01T00:00:00Z",
					Containers: map[string]string{"c0": "docker://old"}},
			),
			wantState: StateSucceeded,
			wantExecd: 0,
		},
		{
			name: "MaxRetriesExceeded_ReturnsFailed",
			pod: podWithRecord(
				newRunningPod("pod1", "docker://old"),
				&restartInfo{ID: sbxID, StartTime: "2026-01-01T00:00:00Z",
					Containers: map[string]string{"c0": "docker://old"},
					RetryCount: int(DefaultMaxRetries)},
			),
			wantState: StateFailed,
			wantExecd: 0,
		},
		{
			name: "RetryIntervalNotElapsed_SkipsExec",
			pod: podWithRecord(
				newRunningPod("pod1", "docker://old"),
				&restartInfo{ID: sbxID, StartTime: "2026-01-01T00:00:00Z",
					Containers:    map[string]string{"c0": "docker://old"},
					LastRetryTime: time.Now().Format(time.RFC3339)},
			),
			wantState: StateRestarting,
			wantExecd: 0,
		},
		{
			name:      "ExecFails_LogsAndContinues",
			pod:       newRunningPod("pod1", "docker://old"),
			execErr:   errors.New("exec failed"),
			wantState: StateRestarting,
			wantExecd: 1,
		},
		{
			// The pod has a record for a different sandbox (previous sandbox restarted
			// successfully, container ID already changed). Handler re-initializes with
			// the new sandbox ID and issues a fresh exec.
			name: "StaleRecord_DifferentID_ReinitializesAndExecs",
			pod: podWithRecord(
				newRunningPod("pod1", "docker://new"),
				&restartInfo{ID: "other-sbx", StartTime: "2026-01-01T00:00:00Z",
					Containers: map[string]string{"c0": "docker://old"}},
			),
			wantState: StateRestarting,
			wantExecd: 1,
		},
		{
			// Retry interval has elapsed after a previous failed attempt: handler execs again.
			name: "RetryIntervalElapsed_Retries",
			pod: podWithRecord(
				newRunningPod("pod1", "docker://old"),
				&restartInfo{ID: sbxID, StartTime: "2026-01-01T00:00:00Z",
					Containers:    map[string]string{"c0": "docker://old"},
					RetryCount:    1,
					LastRetryTime: time.Now().Add(-2 * DefaultRetryInterval).Format(time.RFC3339)},
			),
			wantState: StateRestarting,
			wantExecd: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, stub := newTestHandler(t, tt.pod)
			stub.err = tt.execErr
			ctx := context.Background()

			status, err := h.TryRestart(ctx, pool, tt.pod, opts)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, status.State)
			assert.Len(t, stub.called, tt.wantExecd)
		})
	}
}
