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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
	if fail.Passed || len(fail.Failures) != 8 {
		t.Fatalf("expected 8 failure reasons, got passed=%v failures=%v", fail.Passed, fail.Failures)
	}
}

func newTestDynamicClientWithSandboxCR(t *testing.T, name string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		SandboxGVRV1Alpha2:         "SandboxList",
		SandboxGVRV1Alpha1:         "SandboxList",
		SandboxSnapshotGVRV1Alpha2: "SandboxSnapshotList",
		SandboxSnapshotGVRV1Alpha1: "SandboxSnapshotList",
	})
	_, err := dyn.Resource(SandboxGVRV1Alpha2).Namespace(DefaultResourceNamespace).Create(context.Background(), &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      name,
				"namespace": DefaultResourceNamespace,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed fake sandbox CR: %v", err)
	}
	return dyn
}

func TestLeakAuditorGracePeriodCRDrainPasses(t *testing.T) {
	dyn := newTestDynamicClientWithSandboxCR(t, "draining-sbx-1")
	auditor := NewLeakAuditor(DefaultConfig(), &Clients{DynamicClient: dyn})

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = dyn.Resource(SandboxGVRV1Alpha2).Namespace(DefaultResourceNamespace).Delete(context.Background(), "draining-sbx-1", metav1.DeleteOptions{})
	}()

	res, err := auditor.Audit(context.Background(), LeakAuditOptions{
		GracePeriod:  500 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if !res.Passed || res.ResidualSandboxCRs != 0 || res.CRConvergenceSeconds <= 0 {
		t.Fatalf("expected pass after CR drain with convergence recorded, got %+v", res)
	}
}

func TestLeakAuditorGracePeriodExpiresAndZeroGrace(t *testing.T) {
	dyn := newTestDynamicClientWithSandboxCR(t, "stuck-sbx-1")
	auditor := NewLeakAuditor(DefaultConfig(), &Clients{DynamicClient: dyn})

	// grace=0 checks immediately.
	start := time.Now()
	imm, err := auditor.Audit(context.Background(), LeakAuditOptions{GracePeriod: 0, PollInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if imm.Passed || imm.ResidualSandboxCRs != 1 || imm.CRConvergenceSeconds != 0 || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("expected immediate failure without waiting, got %+v", imm)
	}

	// Non-zero grace expires cleanly when CR never drains.
	exp, err := auditor.Audit(context.Background(), LeakAuditOptions{GracePeriod: 50 * time.Millisecond, PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if exp.Passed || exp.ResidualSandboxCRs != 1 || exp.CRConvergenceSeconds <= 0 {
		t.Fatalf("expected clean failure after grace expiry, got %+v", exp)
	}
}
