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
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestEvaluateLeakAuditPassAndFail(t *testing.T) {
	pass := EvaluateLeakAudit(LeakAuditMeasurements{
		StateRootFreeHeadroomG: 25.0,
		HasStateRootReading:    true,
	}, 10.0)
	if !pass.Passed || len(pass.Failures) != 0 {
		t.Fatalf("expected clean audit to pass, got %+v", pass)
	}

	fail := EvaluateLeakAudit(LeakAuditMeasurements{
		HostOOMKills:           1,
		PodCgroupOOMKills:      2,
		ResidualSandboxCRs:     3,
		ResidualSnapshotCRs:    1,
		StuckDeletingSlots:     2,
		OrphanedUblkDevices:    1,
		LeakedFirecrackerPIDs:  4,
		StateRootFreeHeadroomG: 5.5,
		HasStateRootReading:    true,
	}, 10.0)
	if fail.Passed {
		t.Fatalf("expected dirty audit to fail")
	}
	if len(fail.Failures) != 8 {
		t.Fatalf("expected 8 failure reasons, got %d: %v", len(fail.Failures), fail.Failures)
	}
}

func TestLeakAuditorEndToEnd(t *testing.T) {
	// Fake dynamic client with 1 leaked Sandbox CR and 0 SandboxSnapshot CRs.
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		SandboxGVRV1Alpha2:         "SandboxList",
		SandboxSnapshotGVRV1Alpha2: "SandboxSnapshotList",
	})
	_, err := dyn.Resource(SandboxGVRV1Alpha2).Namespace("opensandbox-dataplane").Create(context.Background(), &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      "leaked-sbx-1",
				"namespace": "opensandbox-dataplane",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed fake sandbox CR: %v", err)
	}

	httpClient := &fakeHTTPDoer{fn: func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("fast_sandbox_fastlet_admission_slots{state=\"deleting\"} 1\n")),
			Header:     make(http.Header),
		}, nil
	}}

	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "firecracker-runtime-n1",
				Namespace: "opensandbox-system",
				Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "firecracker-agent"}, {Name: "overlaybd-ublkd"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "pool-fastlet-0",
				Namespace:   "opensandbox-dataplane",
				Labels:      map[string]string{"fast-sandbox.io/pool": "bench-pool"},
				Annotations: map[string]string{"prometheus.io/port": "9090"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "fastlet"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.20"},
		},
	)

	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		cmdStr := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(cmdStr, "/v1/pool"):
			_, _ = req.Stdout.Write([]byte(`{"enabled":true,"idle":8}`))
		case strings.Contains(cmdStr, "ublk_count="):
			// 10 ublk devices minus 8 warm idle = 2 orphaned.
			_, _ = req.Stdout.Write([]byte("ublk_count=10\nfc_pids=1\noom_kill=5\nstateroot_free_bytes=8589934592\n"))
		case strings.Contains(cmdStr, "MEM_EVENTS"):
			_, _ = req.Stdout.Write([]byte("===MEM_CUR===\n1048576\n===MEM_MAX===\nmax\n===MEM_EVENTS===\noom_kill 1\n"))
		}
		return nil
	}}

	auditor := NewLeakAuditor(DefaultConfig(), &Clients{
		Clientset:     cs,
		DynamicClient: dyn,
		Execer:        execer,
		HTTPClient:    httpClient,
	})

	res, err := auditor.Audit(context.Background(), LeakAuditOptions{
		MinStateRootFreeGiB:  10,
		BaselineHostOOMKills: map[string]int64{"node-1": 5}, // 5 -> 5 => 0 delta
	})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected audit failure, got passed")
	}
	if res.ResidualSandboxCRs != 1 || res.StuckDeletingSlots != 1 || res.OrphanedUblkDevices != 2 || res.LeakedFirecrackerPIDs != 1 || res.PodCgroupOOMKills != 1 || res.HostOOMKills != 0 {
		t.Fatalf("unexpected audit counts: %+v", res)
	}
}

func TestLeakAuditorCaptureBaselineSubtractsHistoricalOOMs(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "firecracker-runtime-abc",
				Namespace: DefaultSystemNamespace,
				Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: DefaultFirecrackerAgentContainer}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		cmdStr := strings.Join(req.Command, " ")
		if strings.Contains(cmdStr, "ublk_count=") {
			_, _ = req.Stdout.Write([]byte("ublk_count=0\nfc_pids=0\noom_kill=76\nstateroot_free_bytes=21474836480\n"))
		} else if strings.Contains(cmdStr, "oom_kill=") {
			_, _ = req.Stdout.Write([]byte("oom_kill=76\n"))
		}
		return nil
	}}
	auditor := NewLeakAuditor(DefaultConfig(), &Clients{Clientset: cs, Execer: execer})
	hostBase, cgBase := auditor.CaptureBaseline(context.Background())
	if hostBase["node-1"] != 76 || cgBase != 0 {
		t.Fatalf("unexpected baseline: host=%v cgroup=%d", hostBase, cgBase)
	}
	res, err := auditor.Audit(context.Background(), LeakAuditOptions{
		BaselineHostOOMKills:   hostBase,
		BaselineCgroupOOMKills: cgBase,
	})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if !res.Passed || res.HostOOMKills != 0 {
		t.Fatalf("expected clean pass with 0 delta host OOM kills, got %+v", res)
	}
}

func TestLeakAuditorPrefersPrometheusForPodCgroupOOM(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-fastlet-0",
			Namespace: DefaultResourceNamespace,
			Labels:    map[string]string{"fast-sandbox.io/pool": "bench-pool"},
		},
		Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "fastlet"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.20"},
	})

	promOOM := "2"
	httpClient := &fakeHTTPDoer{fn: func(_ *http.Request) (*http.Response, error) {
		body := "fast_sandbox_runtime_cgroup_oom_kills_total{sandbox_uid=\"\"} " + promOOM + "\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}}
	execCalls := 0
	execer := &fakeExecer{fn: func(_ context.Context, _ runner.ExecRequest) error {
		execCalls++
		return nil
	}}

	auditor := NewLeakAuditor(DefaultConfig(), &Clients{Clientset: cs, Execer: execer, HTTPClient: httpClient})
	_, cgBase := auditor.CaptureBaseline(context.Background())
	if cgBase != 2 {
		t.Fatalf("expected baseline cgroup OOM=2 from Prometheus, got %d", cgBase)
	}
	promOOM = "5"
	res, err := auditor.Audit(context.Background(), LeakAuditOptions{BaselineCgroupOOMKills: cgBase})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if execCalls != 0 {
		t.Fatalf("expected zero podcgroup exec fallbacks when Prometheus provides OOM counter, got %d", execCalls)
	}
	if res.Passed || res.PodCgroupOOMKills != 3 {
		t.Fatalf("expected PodCgroupOOMKills=3 and audit failure, got %+v", res)
	}
}
