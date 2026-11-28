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

package collector

import (
	"context"
	"math"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

type fakeExecer struct {
	fn func(ctx context.Context, req runner.ExecRequest) error
}

func (f *fakeExecer) Exec(ctx context.Context, req runner.ExecRequest) error {
	return f.fn(ctx, req)
}

func TestOverlayBDProbeSnapshotAndMetrics(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firecracker-runtime-n1",
			Namespace: "opensandbox-system",
			Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "firecracker-agent"},
				{Name: "overlaybd-ublkd"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "overlaybd-ublkd", RestartCount: 1},
			},
		},
	})

	call := 0
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		cmdStr := strings.Join(req.Command, " ")
		if strings.Contains(cmdStr, "/v1/pool") {
			call++
			if call == 1 {
				_, _ = req.Stdout.Write([]byte(`{"enabled":true,"size_gb":2,"low":8,"high":16,"idle":8,"in_use":0,"hits":10,"misses":1}`))
			} else {
				_, _ = req.Stdout.Write([]byte(`{"enabled":true,"size_gb":2,"low":8,"high":16,"idle":6,"in_use":2,"hits":28,"misses":3}`))
			}
			return nil
		}
		_, _ = req.Stdout.Write([]byte("stateroot_free_bytes=53687091200\nstateroot_used_bytes=2147483648\noverlaybd_block_cache_bytes=536870912\nfsb_blob_cache_bytes=1073741824\nfsb_blob_cache_files=14\n"))
		return nil
	}}

	probe := NewOverlayBDProbe(DefaultConfig(), &Clients{Clientset: cs, Execer: execer})
	before, err := probe.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("before snapshot failed: %v", err)
	}
	after, err := probe.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("after snapshot failed: %v", err)
	}
	// Simulate 1 GiB StateRoot growth during the suite.
	n1 := after.Nodes["node-1"]
	n1.Storage.StateRootUsedBytes += bytesPerGiB
	n1.UblkdDaemonRestarts = 2
	after.Nodes["node-1"] = n1

	metrics := ComputeOverlayBDMetrics(before, after, 5, 4)
	if metrics.UblkdPoolHitsDelta != 18 || metrics.UblkdPoolMissesDelta != 2 {
		t.Fatalf("unexpected pool deltas: hits=%d misses=%d", metrics.UblkdPoolHitsDelta, metrics.UblkdPoolMissesDelta)
	}
	if math.Abs(metrics.UblkdPoolHitRate-0.9) > 1e-9 {
		t.Fatalf("expected hit rate 0.9, got %v", metrics.UblkdPoolHitRate)
	}
	if metrics.UblkdPoolIdleMin != 5 || metrics.UblkdPoolIdleEnd != 6 {
		t.Fatalf("unexpected idle min/end: %d/%d", metrics.UblkdPoolIdleMin, metrics.UblkdPoolIdleEnd)
	}
	if metrics.UblkdDaemonRestarts != 1 {
		t.Fatalf("expected restart delta=1, got %d", metrics.UblkdDaemonRestarts)
	}
	if metrics.StateRootDuBytesPerSbx != bytesPerGiB/4 {
		t.Fatalf("expected du per sandbox=%d, got %d", bytesPerGiB/4, metrics.StateRootDuBytesPerSbx)
	}
	if math.Abs(metrics.StateRootFreeHeadroomGi-50.0) > 1e-6 {
		t.Fatalf("expected 50 GiB free headroom, got %v", metrics.StateRootFreeHeadroomGi)
	}
	if after.Nodes["node-1"].Storage.FSBBlobCacheFileCount != 14 {
		t.Fatalf("expected 14 blob cache files, got %d", after.Nodes["node-1"].Storage.FSBBlobCacheFileCount)
	}
	if metrics.OverlayBDBlockCacheBytes != 536870912 {
		t.Fatalf("expected OverlayBDBlockCacheBytes=536870912, got %d", metrics.OverlayBDBlockCacheBytes)
	}
	if metrics.FSBBlobCacheBytes != 1073741824 || metrics.FSBBlobCacheFileCount != 14 {
		t.Fatalf("unexpected blob cache metrics: bytes=%d files=%d", metrics.FSBBlobCacheBytes, metrics.FSBBlobCacheFileCount)
	}
}
