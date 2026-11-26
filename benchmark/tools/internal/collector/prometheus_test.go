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
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeHTTPDoer struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (f *fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return f.fn(req)
}

func TestParsePrometheusText(t *testing.T) {
	input := `
# HELP fast_sandbox_image_affinity_result_total Image affinity outcomes.
# TYPE fast_sandbox_image_affinity_result_total counter
fast_sandbox_image_affinity_result_total{result="hit"} 42
fast_sandbox_image_affinity_result_total{result="miss"} 8 1700000000000

# TYPE fast_sandbox_fastlet_admission_total counter
fast_sandbox_fastlet_admission_total{operation="create",result="denied",reason="mem \"pressure\"\nline2"} 3

# TYPE fast_sandbox_sandbox_proxy_route_latency_seconds histogram
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.01"} 5
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.05"} 9
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="+Inf"} 10
fast_sandbox_sandbox_proxy_route_latency_seconds_sum 0.25
fast_sandbox_sandbox_proxy_route_latency_seconds_count 10

some_nan_gauge NaN
some_inf_gauge +Inf
`
	ms, err := ParsePrometheusText(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParsePrometheusText failed: %v", err)
	}

	if got := ms.Sum("fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"}); got != 42 {
		t.Fatalf("expected hit=42, got %v", got)
	}
	if got := ms.Sum("fast_sandbox_image_affinity_result_total", nil); got != 50 {
		t.Fatalf("expected total=50, got %v", got)
	}

	admSamples := ms.Samples["fast_sandbox_fastlet_admission_total"]
	if len(admSamples) != 1 {
		t.Fatalf("expected 1 admission sample, got %d", len(admSamples))
	}
	wantReason := "mem \"pressure\"\nline2"
	if admSamples[0].Labels["reason"] != wantReason {
		t.Fatalf("unescaped label mismatch: got %q, want %q", admSamples[0].Labels["reason"], wantReason)
	}

	if fam := ms.Families["fast_sandbox_sandbox_proxy_route_latency_seconds"]; fam == nil || fam.Type != MetricTypeHistogram {
		t.Fatalf("expected histogram family metadata, got %+v", fam)
	}
	if !math.IsNaN(ms.Samples["some_nan_gauge"][0].Value) {
		t.Fatalf("expected NaN value")
	}
	if !math.IsInf(ms.Samples["some_inf_gauge"][0].Value, 1) {
		t.Fatalf("expected +Inf value")
	}
}

func TestComputeDeltas(t *testing.T) {
	beforeText := `
fast_sandbox_image_affinity_result_total{result="hit"} 10
fast_sandbox_image_affinity_result_total{result="miss"} 2
fast_sandbox_topk_retry_total{result="conflict"} 5
fast_sandbox_fastlet_admission_total{operation="create",result="allowed",reason="ok"} 100
fast_sandbox_fastlet_admission_total{operation="create",result="denied",reason="memory"} 1
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.01"} 10
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.05"} 20
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="+Inf"} 20
fast_sandbox_sandbox_proxy_route_latency_seconds_sum 0.2
fast_sandbox_sandbox_proxy_route_latency_seconds_count 20
`
	afterText := `
fast_sandbox_image_affinity_result_total{result="hit"} 35
fast_sandbox_image_affinity_result_total{result="miss"} 7
fast_sandbox_topk_retry_total{result="conflict"} 2
fast_sandbox_fastlet_admission_total{operation="create",result="allowed",reason="ok"} 140
fast_sandbox_fastlet_admission_total{operation="create",result="denied",reason="memory"} 4
fast_sandbox_registry_heartbeat_age_seconds 1.25
fast_sandbox_registry_candidate_count 4
fast_sandbox_fastlet_admission_slots{state="running"} 12
fast_sandbox_fastlet_admission_slots{state="deleting"} 0
fast_sandbox_fastlet_resource_admission_state{state="normal",reason="none"} 1
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.01"} 15
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="0.05"} 30
fast_sandbox_sandbox_proxy_route_latency_seconds_bucket{le="+Inf"} 30
fast_sandbox_sandbox_proxy_route_latency_seconds_sum 0.5
fast_sandbox_sandbox_proxy_route_latency_seconds_count 30
`
	beforeMS, err := ParsePrometheusText(strings.NewReader(beforeText))
	if err != nil {
		t.Fatal(err)
	}
	afterMS, err := ParsePrometheusText(strings.NewReader(afterText))
	if err != nil {
		t.Fatal(err)
	}

	deltas := ComputeDeltas(&PromSnapshot{Merged: beforeMS}, &PromSnapshot{Merged: afterMS})

	if got := deltas[`fast_sandbox_image_affinity_result_total{result="hit"}`]; got != 25 {
		t.Fatalf("expected affinity hit delta=25, got %v", got)
	}
	if got := deltas["fast_sandbox_image_affinity_result_total"]; got != 30 {
		t.Fatalf("expected affinity total delta=30, got %v", got)
	}
	// Counter reset (5 -> 2) should clamp to after value (2), not negative.
	if got := deltas[`fast_sandbox_topk_retry_total{result="conflict"}`]; got != 2 {
		t.Fatalf("expected counter-reset delta=2, got %v", got)
	}
	if got := deltas[`fast_sandbox_fastlet_admission_total{operation="create",result="denied",reason="memory"}`]; got != 3 {
		t.Fatalf("expected admission denied delta=3, got %v", got)
	}
	if got := deltas["fast_sandbox_registry_heartbeat_age_seconds_max"]; got != 1.25 {
		t.Fatalf("expected heartbeat age=1.25, got %v", got)
	}
	if got := deltas[`fast_sandbox_fastlet_admission_slots{state="running"}`]; got != 12 {
		t.Fatalf("expected running slots=12, got %v", got)
	}
	if got := deltas["fast_sandbox_sandbox_proxy_route_latency_seconds_count"]; got != 10 {
		t.Fatalf("expected histogram count delta=10, got %v", got)
	}
	if got := deltas["fast_sandbox_sandbox_proxy_route_latency_seconds_avg"]; math.Abs(got-0.03) > 1e-9 {
		t.Fatalf("expected histogram avg=0.03, got %v", got)
	}
	// 5 of 10 delta samples in <=0.01, 5 in (0.01, 0.05] -> P50 = 0.01, P95 = 0.01 + (4.5/5)*0.04 = 0.046
	if got := deltas["fast_sandbox_sandbox_proxy_route_latency_seconds_p50"]; math.Abs(got-0.01) > 1e-9 {
		t.Fatalf("expected histogram p50=0.01, got %v", got)
	}
	if got := deltas["fast_sandbox_sandbox_proxy_route_latency_seconds_p95"]; math.Abs(got-0.046) > 1e-9 {
		t.Fatalf("expected histogram p95=0.046, got %v", got)
	}
}

func TestPrometheusScraperDiscoverAndGracefulDegradation(t *testing.T) {
	httpClient := &fakeHTTPDoer{fn: func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Host, "10.0.0.10") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("fast_sandbox_image_affinity_result_total{result=\"hit\"} 7\n")),
				Header:     make(http.Header),
			}, nil
		}
		return nil, fmt.Errorf("connection refused")
	}}

	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fast-sandbox-controller-abc",
				Namespace: "opensandbox-system",
				Labels:    map[string]string{"app": "fast-sandbox-controller"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "manager",
					Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9091}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.10"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pool-fastlet-0",
				Namespace: "opensandbox-dataplane",
				Labels:    map[string]string{"fast-sandbox.io/pool": "bench-pool"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "fastlet", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}}},
					{Name: "fastlet-proxy"},
					{Name: "egress"},
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.20"},
		},
	)

	scraper := NewPrometheusScraper(DefaultConfig(), &Clients{Clientset: cs, HTTPClient: httpClient})
	targets, warns := scraper.DiscoverTargets(context.Background())
	if len(warns) != 0 {
		t.Fatalf("unexpected discovery warnings: %v", warns)
	}
	// Expect controller (1) + fastlet (1) + fastlet-proxy (1) + egress (1) = 4 targets.
	if len(targets) != 4 {
		t.Fatalf("expected 4 discovered targets, got %d: %+v", len(targets), targets)
	}

	// Scrape one live target and one unreachable target to verify graceful degradation.
	snap := scraper.ScrapeTargets(context.Background(), []ScrapeTarget{
		{Component: ComponentFSBController, Namespace: "opensandbox-system", PodName: "ctrl-0", URL: "http://10.0.0.10:9091/metrics"},
		{Component: ComponentFSBFastlet, Namespace: "opensandbox-dataplane", PodName: "fastlet-0", URL: "http://10.0.0.20:9090/metrics"},
	})
	if len(snap.Warnings) != 1 {
		t.Fatalf("expected 1 warning for failed target, got %v", snap.Warnings)
	}
	if got := snap.Merged.Sum("fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"}); got != 7 {
		t.Fatalf("expected scraped metric=7 despite partial failure, got %v", got)
	}
}
