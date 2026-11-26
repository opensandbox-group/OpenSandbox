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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestEvictOverlayBDCachesAndRewarm(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firecracker-runtime-n1",
			Namespace: "opensandbox-system",
			Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "overlaybd-ublkd"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	evicted := false
	poolPolls := 0
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		cmdStr := strings.Join(req.Command, " ")
		if strings.Contains(cmdStr, "shared_snapfiles") {
			evicted = true
			return nil
		}
		if strings.Contains(cmdStr, "/v1/pool") {
			poolPolls++
			if poolPolls == 1 {
				_, _ = req.Stdout.Write([]byte(`{"enabled":true,"idle":3}`))
			} else {
				_, _ = req.Stdout.Write([]byte(`{"enabled":true,"idle":8}`))
			}
			return nil
		}
		return nil
	}}

	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs, Execer: execer})
	err := ops.EvictOverlayBDCaches(context.Background(), EvictCacheOptions{
		PoolLow:       8,
		RewarmTimeout: 2 * time.Second,
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("EvictOverlayBDCaches failed: %v", err)
	}
	if !evicted || poolPolls != 2 {
		t.Fatalf("expected eviction + 2 pool polls, got evicted=%v polls=%d", evicted, poolPolls)
	}
}

func TestIsolateAndRestoreFastletNode(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-node1",
				Namespace: "opensandbox-dataplane",
				Labels:    map[string]string{"fast-sandbox.io/pool": "bench-pool"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "fastlet"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-node2",
				Namespace: "opensandbox-dataplane",
				Labels:    map[string]string{"fast-sandbox.io/pool": "bench-pool"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-2",
				Containers: []corev1.Container{{Name: "fastlet"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)

	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs})
	state, err := ops.IsolateFastletNode(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("IsolateFastletNode failed: %v", err)
	}
	if !state.TaintAdded {
		t.Fatalf("expected taint to be added")
	}
	if len(state.DeletedPods) != 1 || state.DeletedPods[0] != "fastlet-node1" {
		t.Fatalf("expected only fastlet-node1 deleted, got %v", state.DeletedPods)
	}
	// Verify node-2's fastlet pod was untouched.
	if _, err := cs.CoreV1().Pods("opensandbox-dataplane").Get(context.Background(), "fastlet-node2", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected fastlet-node2 to remain untouched: %v", err)
	}

	node, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != IsolationTaintKey {
		t.Fatalf("expected isolation taint on node-1, got %+v", node.Spec.Taints)
	}

	if err := ops.RestoreFastletNode(context.Background(), "node-1"); err != nil {
		t.Fatalf("RestoreFastletNode failed: %v", err)
	}
	node, _ = cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if len(node.Spec.Taints) != 0 {
		t.Fatalf("expected isolation taint removed, got %+v", node.Spec.Taints)
	}
}

func TestTriggerRegistryGC(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "opensandbox-bench-registry-0",
			Namespace: "opensandbox-bench",
			Labels:    map[string]string{"app": "opensandbox-bench-registry"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "registry"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	var ranCmd []string
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		ranCmd = req.Command
		return nil
	}}

	ops := NewClusterOps(DefaultConfig(), &Clients{Clientset: cs, Execer: execer})
	if err := ops.TriggerRegistryGC(context.Background(), "opensandbox-bench"); err != nil {
		t.Fatalf("TriggerRegistryGC failed: %v", err)
	}
	if len(ranCmd) < 2 || ranCmd[0] != "registry" || ranCmd[1] != "garbage-collect" {
		t.Fatalf("unexpected GC command: %v", ranCmd)
	}
}
