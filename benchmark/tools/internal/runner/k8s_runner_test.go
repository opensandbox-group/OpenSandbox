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

package runner

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

type fakeExecer struct {
	reqs []ExecRequest
	fn   func(ctx context.Context, req ExecRequest) error
}

func (f *fakeExecer) Exec(ctx context.Context, req ExecRequest) error {
	f.reqs = append(f.reqs, req)
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return nil
}

func TestBuildRunnerPodSpecAndAffinity(t *testing.T) {
	prof, err := profile.LoadEmbedded(profile.DefaultProfileName)
	require.NoError(t, err)

	pod, err := BuildRunnerPodSpec("opensandbox-bench", DefaultRunnerPodName, prof)
	require.NoError(t, err)
	require.Equal(t, "alpine:3.20", pod.Spec.Containers[0].Image)
	require.Equal(t, "4", pod.Spec.Containers[0].Resources.Limits.Cpu().String())
	require.Equal(t, "4Gi", pod.Spec.Containers[0].Resources.Limits.Memory().String())

	terms := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	expr := terms[0].Preference.MatchExpressions[0]
	require.Equal(t, FirecrackerNodeLabelKey, expr.Key)
	require.Equal(t, corev1.NodeSelectorOpNotIn, expr.Operator)
	require.Equal(t, []string{"true"}, expr.Values)
}

func TestEnsureRunnerPodReusesReadyAndRecreatesFailed(t *testing.T) {
	prof, err := profile.LoadEmbedded(profile.DefaultProfileName)
	require.NoError(t, err)

	t.Run("reuses ready running pod", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-bench"}},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: DefaultRunnerPodName, Namespace: "opensandbox-bench"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
		)
		r := &K8sRunner{Clientset: cs, Logger: slog.Default(), PollInterval: 5 * time.Millisecond, ReadyTimeout: time.Second}
		podName, err := r.EnsureRunnerPod(context.Background(), prof)
		require.NoError(t, err)
		require.Equal(t, DefaultRunnerPodName, podName)
	})

	t.Run("creates namespace and waits for ready", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		r := &K8sRunner{Clientset: cs, Logger: slog.Default(), PollInterval: 10 * time.Millisecond, ReadyTimeout: 2 * time.Second}

		go func() {
			for i := 0; i < 50; i++ {
				time.Sleep(15 * time.Millisecond)
				pod, err := cs.CoreV1().Pods("opensandbox-bench").Get(context.Background(), DefaultRunnerPodName, metav1.GetOptions{})
				if err != nil {
					continue
				}
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				_, _ = cs.CoreV1().Pods("opensandbox-bench").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
				return
			}
		}()

		podName, err := r.EnsureRunnerPod(context.Background(), prof)
		require.NoError(t, err)
		require.Equal(t, DefaultRunnerPodName, podName)
	})
}

func TestDetectClusterArch(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-arm",
				Labels: map[string]string{corev1.LabelArchStable: "arm64"},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultRunnerPodName, Namespace: "opensandbox-bench"},
			Spec:       corev1.PodSpec{NodeName: "node-arm"},
		},
	)
	r := &K8sRunner{Clientset: cs, Logger: slog.Default()}
	arch, err := r.DetectClusterArch(context.Background(), "opensandbox-bench", DefaultRunnerPodName)
	require.NoError(t, err)
	require.Equal(t, "arm64", arch)
}

func TestInjectBinaryExecWorkerAndPullBundle(t *testing.T) {
	binFile := filepath.Join(t.TempDir(), "opensandbox-bench-linux")
	require.NoError(t, os.WriteFile(binFile, []byte("ELF-FAKE"), 0o755))

	var uploaded bytes.Buffer
	execer := &fakeExecer{
		fn: func(_ context.Context, req ExecRequest) error {
			if req.Stdin != nil {
				_, err := io.Copy(&uploaded, req.Stdin)
				return err
			}
			if len(req.Command) > 0 && req.Command[0] == "tar" {
				return WriteBytesTar(req.Stdout, "report.json", []byte(`{"schema_version":2}`), 0o644)
			}
			if req.Stdout != nil {
				_, _ = io.WriteString(req.Stdout, "worker ok\n")
			}
			return nil
		},
	}

	r := &K8sRunner{Clientset: fake.NewSimpleClientset(), Execer: execer, Logger: slog.Default()}

	require.NoError(t, r.InjectBinary(context.Background(), "opensandbox-bench", DefaultRunnerPodName, binFile, DefaultRemoteBinaryPath))
	extracted := t.TempDir()
	require.NoError(t, ExtractTar(&uploaded, extracted))
	gotBin, err := os.ReadFile(filepath.Join(extracted, "opensandbox-bench"))
	require.NoError(t, err)
	require.Equal(t, []byte("ELF-FAKE"), gotBin)

	var stdout, stderr bytes.Buffer
	require.NoError(t, r.ExecWorker(context.Background(), "opensandbox-bench", DefaultRunnerPodName, DefaultRemoteBinaryPath, []string{"worker", "run"}, &stdout, &stderr))
	require.Equal(t, "worker ok\n", stdout.String())

	outDir := t.TempDir()
	require.NoError(t, r.PullBundle(context.Background(), "opensandbox-bench", DefaultRunnerPodName, DefaultRemoteBundleDir, outDir))
	gotReport, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"schema_version":2}`, string(gotReport))
}
