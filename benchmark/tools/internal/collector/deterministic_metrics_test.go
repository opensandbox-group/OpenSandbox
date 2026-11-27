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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestSampleHostNodePrefersJanitorAndNsenterConntrack(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firecracker-runtime-n1",
			Namespace: DefaultSystemNamespace,
			Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: DefaultFirecrackerAgentContainer},
				{Name: DefaultUblkdContainer},
				{Name: DefaultJanitorContainer},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	var seenContainer string
	var seenCmd string
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		seenContainer = req.Container
		seenCmd = strings.Join(req.Command, " ")
		_, _ = req.Stdout.Write([]byte(`
===STAT===
cpu  100 0 50 850 0 0 0 0
===LOADAVG===
1.25 0.90 0.80 1/200 1234
===MEMINFO===
MemTotal:       65536000 kB
MemAvailable:   40960000 kB
Buffers:          512000 kB
Cached:          2048000 kB
Slab:            1024000 kB
SReclaimable:     512000 kB
SUnreclaim:       512000 kB
===CONNTRACK===
unavailable
unavailable
===UBLK_COUNT===
12
===FC_PIDS===
4
`))
		return nil
	}}

	sampler := NewLoadSampler(DefaultConfig(), &Clients{
		Clientset: fake.NewSimpleClientset(pod),
		Execer:    execer,
	}, time.Second)

	hs, warn := sampler.sampleHostNode(context.Background(), pod)
	if warn != "" {
		t.Fatalf("unexpected warn: %s", warn)
	}
	if seenContainer != DefaultJanitorContainer {
		t.Fatalf("expected janitor container, got %q", seenContainer)
	}
	if !strings.Contains(seenCmd, "nsenter -t 1 -n") {
		t.Fatalf("expected nsenter -t 1 -n in conntrack probe, got %s", seenCmd)
	}
	if hs.ActiveUblkDevices != 12 {
		t.Fatalf("expected 12 ublk devices, got %d", hs.ActiveUblkDevices)
	}
	if !hs.ConntrackUnavailable {
		t.Fatalf("expected ConntrackUnavailable=true when probe outputs unavailable")
	}
	if hs.MemTotalMiB != 64000 || hs.SlabMiB != 1000 || hs.SUnreclaimMiB != 500 {
		t.Fatalf("unexpected meminfo fields: total=%v slab=%v sunreclaim=%v", hs.MemTotalMiB, hs.SlabMiB, hs.SUnreclaimMiB)
	}

	sampler.RecordSample(LoadSample{Hosts: []HostSample{*hs}})
	lp := sampler.Aggregate()
	if lp.Host == nil || lp.Host.Metrics["conntrack_status"] != "unavailable" {
		t.Fatalf("expected conntrack_status=unavailable in aggregated host metrics, got %+v", lp.Host)
	}
}

func TestRunTelemetryPreflightOKAndDegraded(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		SandboxGVRV1Alpha2: "SandboxList",
		SandboxGVRV1Alpha1: "SandboxList",
	})
	rtPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firecracker-runtime-0",
			Namespace: DefaultSystemNamespace,
			Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: DefaultJanitorContainer}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}, rtPod)
	okExec := &fakeExecer{fn: func(_ context.Context, _ runner.ExecRequest) error { return nil }}

	res := RunTelemetryPreflight(context.Background(), DefaultConfig(), &Clients{
		Clientset:     cs,
		DynamicClient: dyn,
		Execer:        okExec,
	})
	if !res.OK() || res.Status != TelemetryStatusOK {
		t.Fatalf("expected preflight OK, got %+v", res)
	}

	// Simulate RBAC 403 on nodes:list and exec failure.
	badCS := fake.NewSimpleClientset(rtPod)
	badCS.PrependReactor("list", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden: nodes cannot be listed")
	})
	badExec := &fakeExecer{fn: func(_ context.Context, _ runner.ExecRequest) error {
		return errors.New("forbidden: pods/exec")
	}}
	badRes := RunTelemetryPreflight(context.Background(), DefaultConfig(), &Clients{
		Clientset:     badCS,
		DynamicClient: dyn,
		Execer:        badExec,
	})
	if badRes.OK() || badRes.Status != TelemetryStatusDegraded || len(badRes.Missing) < 2 {
		t.Fatalf("expected degraded preflight with >=2 missing items, got %+v", badRes)
	}
}

func TestLeakAuditorProbeFailureNeverPassesAndCleanSlotsConservation(t *testing.T) {
	t.Run("probe failure marks audit incomplete and failed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "firecracker-runtime-n1",
				Namespace: DefaultSystemNamespace,
				Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: DefaultJanitorContainer}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
		execer := &fakeExecer{fn: func(_ context.Context, _ runner.ExecRequest) error {
			return errors.New("403 forbidden")
		}}
		auditor := NewLeakAuditor(DefaultConfig(), &Clients{Clientset: cs, Execer: execer})
		res, err := auditor.Audit(context.Background(), LeakAuditOptions{})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Passed || !res.Incomplete || len(res.Warnings) == 0 {
			t.Fatalf("expected Incomplete=true and Passed=false on exec failure, got %+v", res)
		}
		foundProbeFail := false
		for _, f := range res.Failures {
			if strings.Contains(f, "audit_probe_failed") {
				foundProbeFail = true
			}
		}
		if !foundProbeFail {
			t.Fatalf("expected audit_probe_failed in Failures, got %v", res.Failures)
		}
	})

	t.Run("cleanSlots conservation gate fails on lost clean slots or non-clean residuals", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pool-fastlet-0",
				Namespace: DefaultResourceNamespace,
				Labels:    map[string]string{"fast-sandbox.io/pool": "bench-pool"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "fastlet"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.21"},
		})

		promBody := "fast_sandbox_fastlet_admission_slots{state=\"clean\"} 64\n"
		httpClient := &fakeHTTPDoer{fn: func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(promBody)),
				Header:     make(http.Header),
			}, nil
		}}

		auditor := NewLeakAuditor(DefaultConfig(), &Clients{Clientset: cs, HTTPClient: httpClient})
		base := auditor.CaptureFullBaseline(context.Background())
		if base.CleanSlots != 64 {
			t.Fatalf("expected baseline clean slots=64, got %+v", base)
		}

		// Simulate lost clean slots (64 -> 60) and 1 residual dirty slot.
		promBody = "fast_sandbox_fastlet_admission_slots{state=\"clean\"} 60\nfast_sandbox_fastlet_admission_slots{state=\"dirty\"} 1\n"
		res, err := auditor.Audit(context.Background(), LeakAuditOptions{
			BaselineCleanSlots: base.CleanSlots,
		})
		if err != nil {
			t.Fatalf("Audit err: %v", err)
		}
		if res.Passed {
			t.Fatalf("expected failure when clean slots lost and dirty slot remains")
		}
		if res.BaselineCleanSlots != 64 || res.CleanSlotsAfter != 60 || res.LostCleanSlots != 4 || res.NonCleanResidualSlots != 1 {
			t.Fatalf("unexpected slot conservation fields: %+v", res)
		}
	})
}

func TestTakeHostMemoryAndProcessSnapshot(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firecracker-runtime-n1",
			Namespace: DefaultSystemNamespace,
			Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: DefaultFirecrackerAgentContainer}, {Name: DefaultJanitorContainer}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	var seenContainer string
	execer := &fakeExecer{fn: func(_ context.Context, req runner.ExecRequest) error {
		seenContainer = req.Container
		_, _ = req.Stdout.Write([]byte(`
===STAT===
cpu  200 0 100 700 0 0 0 0
===LOADAVG===
3.50 2.10 1.80 2/300 4567
===MEMINFO===
MemTotal:       65536000 kB
MemAvailable:   50176000 kB
Buffers:          262144 kB
Cached:          1048576 kB
Slab:            1536000 kB
SReclaimable:     512000 kB
SUnreclaim:      1024000 kB
===PSI_CPU===
some avg10=1.00 avg60=2.50 avg300=1.20 total=100
===PSI_MEM===
full avg10=0.00 avg60=0.15 avg300=0.05 total=10
===FC_SMAPS===
fc_pid_count=10
fc_pss_kib=409600
fc_rss_kib=614400
===DAEMON_RSS===
fastlet_rss_kib=102400
egress_rss_kib=51200
ublkd_rss_kib=204800
proxy_rss_kib=30720
`))
		return nil
	}}

	sampler := NewLoadSampler(DefaultConfig(), &Clients{
		Clientset: fake.NewSimpleClientset(pod),
		Execer:    execer,
	}, time.Second)

	snap, err := sampler.TakeHostMemoryAndProcessSnapshot(context.Background())
	if err != nil {
		t.Fatalf("TakeHostMemoryAndProcessSnapshot failed: %v", err)
	}
	if seenContainer != DefaultJanitorContainer {
		t.Fatalf("expected janitor container, got %q", seenContainer)
	}
	if snap.FCPIDCount != 10 || snap.FCPssAvgMiB != 40 || snap.FCRssAvgMiB != 60 {
		t.Fatalf("unexpected firecracker PSS/RSS: %+v", snap)
	}
	if snap.FastletRSSMiB != 100 || snap.EgressRSSMiB != 50 || snap.UblkdRSSMiB != 200 || snap.ProxyRSSMiB != 30 {
		t.Fatalf("unexpected daemon RSS: %+v", snap)
	}
	if snap.SlabMiB != 1500 || snap.SUnreclaimMiB != 1000 || snap.Load1m != 3.50 {
		t.Fatalf("unexpected host memory/load: %+v", snap)
	}
}

func TestBuildStageAttributionFromPromAndClientLatencies(t *testing.T) {
	beforeText := `
opensandbox_sandbox_create_duration_milliseconds_bucket{le="100"} 0
opensandbox_sandbox_create_duration_milliseconds_bucket{le="500"} 0
opensandbox_sandbox_create_duration_milliseconds_bucket{le="+Inf"} 0
opensandbox_sandbox_create_duration_milliseconds_sum 0
opensandbox_sandbox_create_duration_milliseconds_count 0
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="0.1"} 0
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="0.5"} 0
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="+Inf"} 0
fast_sandbox_create_stage_latency_seconds_sum{stage="vmm_boot"} 0
fast_sandbox_create_stage_latency_seconds_count{stage="vmm_boot"} 0
fast_sandbox_fastlet_proxy_upstream_latency_seconds_bucket{le="0.05"} 0
fast_sandbox_fastlet_proxy_upstream_latency_seconds_bucket{le="+Inf"} 0
fast_sandbox_fastlet_proxy_upstream_latency_seconds_sum 0
fast_sandbox_fastlet_proxy_upstream_latency_seconds_count 0
`
	afterText := `
opensandbox_sandbox_create_duration_milliseconds_bucket{le="100"} 5
opensandbox_sandbox_create_duration_milliseconds_bucket{le="500"} 10
opensandbox_sandbox_create_duration_milliseconds_bucket{le="+Inf"} 10
opensandbox_sandbox_create_duration_milliseconds_sum 2000
opensandbox_sandbox_create_duration_milliseconds_count 10
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="0.1"} 5
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="0.5"} 10
fast_sandbox_create_stage_latency_seconds_bucket{stage="vmm_boot",le="+Inf"} 10
fast_sandbox_create_stage_latency_seconds_sum{stage="vmm_boot"} 1.5
fast_sandbox_create_stage_latency_seconds_count{stage="vmm_boot"} 10
fast_sandbox_fastlet_proxy_upstream_latency_seconds_bucket{le="0.05"} 10
fast_sandbox_fastlet_proxy_upstream_latency_seconds_bucket{le="+Inf"} 10
fast_sandbox_fastlet_proxy_upstream_latency_seconds_sum 0.2
fast_sandbox_fastlet_proxy_upstream_latency_seconds_count 10
`
	bMS, err := ParsePrometheusText(strings.NewReader(beforeText))
	if err != nil {
		t.Fatal(err)
	}
	aMS, err := ParsePrometheusText(strings.NewReader(afterText))
	if err != nil {
		t.Fatal(err)
	}
	deltas := ComputeDeltas(&PromSnapshot{Merged: bMS}, &PromSnapshot{Merged: aMS})
	latencies := map[string]report.LatencyDist{
		"t_api_ack":          {Count: 10, P50Ms: 120, P99Ms: 300},
		"t_endpoint_resolve": {Count: 10, P50Ms: 140, P99Ms: 350},
		"t_first_ping":       {Count: 10, P50Ms: 250, P99Ms: 500},
	}

	stages := BuildStageAttribution(deltas, latencies)
	wantSpans := map[string]bool{
		"client.api_ack":               false,
		"client.endpoint_resolve":      false,
		"client.first_ping_wait":       false,
		"server.sandbox_create":        false,
		"fast_sandbox.create.vmm_boot": false,
		"fastlet_proxy.upstream":       false,
	}
	for _, st := range stages {
		if _, ok := wantSpans[st.SpanName]; ok {
			wantSpans[st.SpanName] = true
		}
	}
	for span, seen := range wantSpans {
		if !seen {
			t.Fatalf("missing expected stage attribution %q in %+v", span, stages)
		}
	}
}
