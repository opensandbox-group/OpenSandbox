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

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// transitionExport initializes the production telemetry pipeline against a
// local OTLP receiver. Every observation asserted below travels through the
// real worker, the real instruments and the OTLP exporter; nothing is mocked.
type transitionExport struct {
	t        *testing.T
	received chan []byte
}

func captureTransitions(t *testing.T) *transitionExport {
	t.Helper()
	e := &transitionExport{t: t, received: make(chan []byte, 64)}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		e.received <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", endpoint.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "cumulative")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "600000")
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	shutdown, err := telemetry.Init(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, shutdown(context.Background()))
		otel.SetMeterProvider(previousMeter)
		otel.SetTracerProvider(previousTracer)
		endpoint.Close()
	})
	return e
}

// collect flushes and returns cumulative "transition/result" counts. It checks
// that every exported point carries only the two closed labels besides the
// shared process attributes and that no forbidden test value (subject, host,
// socket, token, error text) is exported. consistent reports whether counter
// and histogram agree: they are two SDK calls, so a flush taken while the
// worker is between them may see one ahead of the other.
func (e *transitionExport) collect(forbidden ...string) map[string]int64 {
	counts, _ := e.collectConsistent(forbidden...)
	return counts
}

func (e *transitionExport) collectConsistent(forbidden ...string) (map[string]int64, bool) {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, telemetry.ForceFlush(ctx))
	var last []byte
	for drained := false; !drained; {
		select {
		case body := <-e.received:
			last = body
		default:
			drained = true
		}
	}
	counts := map[string]int64{}
	if last == nil {
		return counts, true
	}
	for _, value := range forbidden {
		require.False(t, bytes.Contains(last, []byte(value)), "exported metrics contain %q", value)
	}
	var req collectormetrics.ExportMetricsServiceRequest
	require.NoError(t, proto.Unmarshal(last, &req))
	transitions := map[string]bool{"register": true, "install": true, "suspend": true, "unload": true, "replay": true}
	results := map[string]bool{"ok": true, "rejected": true, "indeterminate": true, "failed": true}
	observed := map[string]uint64{}
	for _, resource := range req.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				var points []map[string]string
				switch metric.Name {
				case "egress.credential_vault.transitions_total":
					require.True(t, metric.GetSum().IsMonotonic)
					for _, point := range metric.GetSum().DataPoints {
						attrs := transitionAttrs(t, point.Attributes)
						points = append(points, attrs)
						counts[attrs["transition"]+"/"+attrs["result"]] += point.GetAsInt()
					}
				case "egress.credential_vault.transition.duration":
					require.Equal(t, "s", metric.Unit)
					for _, point := range metric.GetHistogram().DataPoints {
						attrs := transitionAttrs(t, point.Attributes)
						points = append(points, attrs)
						observed[attrs["transition"]+"/"+attrs["result"]] += point.Count
						require.GreaterOrEqual(t, point.GetSum(), 0.0)
					}
				}
				for _, attrs := range points {
					require.True(t, transitions[attrs["transition"]], "unexpected transition %q", attrs["transition"])
					require.True(t, results[attrs["result"]], "unexpected result %q", attrs["result"])
				}
			}
		}
	}
	consistent := len(observed) == len(counts)
	for key, count := range counts {
		consistent = consistent && uint64(count) == observed[key]
	}
	return counts, consistent
}

func transitionAttrs(t *testing.T, raw []*commonv1.KeyValue) map[string]string {
	t.Helper()
	attrs := map[string]string{}
	for _, kv := range raw {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	for key := range attrs {
		switch key {
		case "transition", "result", "sandbox_id":
		default:
			t.Fatalf("unexpected transition attribute %q", key)
		}
	}
	return attrs
}

// waitFor waits until the exported counts satisfy done with counter and
// histogram in agreement. Each worker observation is recorded after its result
// has been applied, so it can trail the receiver state slightly.
func (e *transitionExport) waitFor(done func(map[string]int64) bool, forbidden ...string) map[string]int64 {
	e.t.Helper()
	var last map[string]int64
	require.Eventually(e.t, func() bool {
		counts, consistent := e.collectConsistent(forbidden...)
		last = counts
		return consistent && done(counts)
	}, 5*time.Second, 20*time.Millisecond, "transition counts")
	return last
}

// waitTransitions waits until the exported counts equal want exactly.
func (e *transitionExport) waitTransitions(want map[string]int64, forbidden ...string) {
	e.t.Helper()
	last := e.waitFor(func(counts map[string]int64) bool { return equalCounts(counts, want) }, forbidden...)
	require.Equal(e.t, want, last)
}

func equalCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// Control outcomes compared across the admission flag. waitLifecycleIdle
// additionally pins identical receiver lifecycle sequences in both modes.
type lifecycleScenarioResult struct {
	degraded            []string
	ready               bool
	replayDecisionEpoch int64
}

// Drives register, install, unload, child replacement replay and suspend
// through the production worker and the production addon receiver.
func runLifecycleMetricsScenario(t *testing.T, admission bool) (lifecycleScenarioResult, map[string]int64) {
	export := captureTransitions(t)
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	o.cfg.tlsAdmission = admission
	launch, configs := subjectAddonLauncher(t, o)
	first, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(first)
	require.Equal(t, admission, o.sessions[first].handoff.TLSAdmission)
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	s.publishSubjectClient(o.sessions[first].client, o.sessions[first].handoff)
	h := (*configs)[0].SubjectRevisionIPC
	const alpha, beta = "metrics-alpha-7f3a", "metrics-beta-91c2"
	const runtimeID, attachmentID, betaRuntime = "metrics-runtime-5d1e", "metrics-attachment-0b6c", "metrics-runtime-8e2f"
	forbidden := []string{alpha, beta, "s-" + alpha, "10.77.0.11", "10.77.0.12", runtimeID, attachmentID, betaRuntime,
		"api.example.com", "sekret", h.SocketPath, h.SessionToken, h.ControlGeneration}
	require.Equal(t, 200, doAction(t, s, bindBody(t, alpha, "10.77.0.11", runtimeID, attachmentID, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, beta, "10.77.0.12", betaRuntime, attachmentID, 1, vaultAllowPolicy())).Code)
	waitSubjectInstalled(t, h, lifecycleTarget(t, alpha, runtimeID, attachmentID), latestSubjectSnapshot(t, s, alpha))
	waitSubjectInstalled(t, h, lifecycleTarget(t, beta, betaRuntime, attachmentID), latestSubjectSnapshot(t, s, beta))
	before := latestSubjectSnapshot(t, s, beta)
	require.Equal(t, 201, doVault(t, s, http.MethodPost, beta, vaultCredential(), "").Code)
	require.NotEqual(t, before.digest, latestSubjectSnapshot(t, s, beta).digest)
	waitSubjectInstalled(t, h, lifecycleTarget(t, beta, betaRuntime, attachmentID), latestSubjectSnapshot(t, s, beta))
	require.Equal(t, 200, doAction(t, s, removeBody(t, alpha, runtimeID, attachmentID)).Code)
	waitSubjectKnown(t, h, lifecycleTarget(t, alpha, runtimeID, attachmentID), false)
	waitLifecycleIdle(t, s, h, 3)
	var result lifecycleScenarioResult

	// Child replacement: publication rebuilds Register and install for the
	// surviving subject only, both marked as replay.
	second, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(second)
	next := (*configs)[1].SubjectRevisionIPC
	forbidden = append(forbidden, next.SocketPath, next.SessionToken, next.ControlGeneration)
	s.publishSubjectClient(o.sessions[second].client, o.sessions[second].handoff)
	active := waitSubjectInstalled(t, next, lifecycleTarget(t, beta, betaRuntime, attachmentID), latestSubjectSnapshot(t, s, beta))
	result.replayDecisionEpoch = active.DecisionEpoch
	waitSubjectKnown(t, next, lifecycleTarget(t, alpha, runtimeID, attachmentID), false)
	waitLifecycleIdle(t, s, next, 1)

	// Authority loss through the real Vault entry publication seam.
	subj := subject.FromSandboxUID(beta)
	s.mu.Lock()
	entry := s.vaults[subj]
	entry.vaultStateUnknown = true
	s.setSubjectVaultEntryLocked(subj, entry)
	s.mu.Unlock()
	waitLifecycleIdle(t, s, next, 2)
	result.degraded = s.subjectLifecycleDegraded()
	result.ready = s.actionStatusResponse().Ready

	if admission {
		export.waitTransitions(map[string]int64{
			"register/ok": 2, "install/ok": 3, "unload/ok": 1, "replay/ok": 2, "suspend/ok": 1,
		}, forbidden...)
	} else {
		// Nothing is recorded asynchronously either: the receiver is idle.
		require.Empty(t, export.collect(forbidden...))
	}
	return result, export.collect(forbidden...)
}

func TestFastSandboxSubjectMetricsLifecycleAndAdmissionDifferential(t *testing.T) {
	results := map[bool]lifecycleScenarioResult{}
	for _, admission := range []bool{false, true} {
		name := "admission-off"
		if admission {
			name = "admission-on"
		}
		t.Run(name, func(t *testing.T) {
			result, counts := runLifecycleMetricsScenario(t, admission)
			if !admission {
				require.Empty(t, counts, "revision-only children record no transition")
			}
			results[admission] = result
		})
	}
	require.Len(t, results, 2)
	// The observation flag must not change any control outcome.
	require.Equal(t, results[false], results[true])
	require.Equal(t, int64(1), results[true].replayDecisionEpoch)
	require.Empty(t, results[true].degraded)
	require.True(t, results[true].ready)
}

// Classifier boundaries the receiver fixtures cannot reach deterministically:
// local validation failures and the confirmed suspend no-op. Real-path
// coverage of every other row follows below.
func TestFastSandboxSubjectMetricsClassifiers(t *testing.T) {
	never := func() bool { t.Fatal("Pending consulted for a confirmed receipt"); return false }
	confirmed := func(outcome string) revision.LifecycleReceipt {
		return revision.LifecycleReceipt{LifecycleSequence: 4, Result: revision.LifecycleResult{HTTPStatus: 409, Outcome: outcome}}
	}
	register, suspend := fastSandboxSubjectIntent{}, fastSandboxSubjectIntent{suspend: true}
	require.Equal(t, "ok", subjectLifecycleResult(register, revision.LifecycleReceipt{LifecycleSequence: 1}, nil, never))
	for _, outcome := range []string{"subject_unknown", "subject_generation_mismatch"} {
		require.Equal(t, "ok", subjectLifecycleResult(suspend, confirmed(outcome), revision.ErrTransportRejected, never))
		require.Equal(t, "rejected", subjectLifecycleResult(register, confirmed(outcome), revision.ErrTransportRejected, never))
	}
	require.Equal(t, "rejected", subjectLifecycleResult(suspend, confirmed("subject_failed"), revision.ErrTransportRejected, never))
	// Error text is never parsed: a confirmed receipt decides, then Pending.
	require.Equal(t, "rejected", subjectLifecycleResult(register, confirmed("x"), errors.New("timeout"), never))
	require.Equal(t, "indeterminate", subjectLifecycleResult(register, revision.LifecycleReceipt{}, revision.ErrTransportRejected, func() bool { return true }))
	require.Equal(t, "failed", subjectLifecycleResult(register, revision.LifecycleReceipt{}, revision.ErrIndeterminate, func() bool { return false }))
	require.Equal(t, "failed", subjectLifecycleResult(register, revision.LifecycleReceipt{}, revision.ErrInvalidTransport, func() bool { return false }))

	plain := &fastSandboxSubjectInstaller{transport: &fastSandboxInstallTransport{}}
	rejected := &fastSandboxSubjectInstaller{transport: &fastSandboxInstallTransport{rejection: &revision.SubjectTransportRejection{Outcome: "subject_failed"}}}
	require.Equal(t, "ok", subjectInstallResult(plain, nil, true))
	require.Equal(t, "rejected", subjectInstallResult(rejected, revision.ErrIndeterminate, false), "structured rejection wins")
	require.Equal(t, "rejected", subjectInstallResult(rejected, revision.ErrInvalid, false))
	require.Equal(t, "failed", subjectInstallResult(plain, revision.ErrInvalid, false))
	require.Equal(t, "failed", subjectInstallResult(plain, revision.ErrPrepareRejected, false))
	require.Equal(t, "failed", subjectInstallResult(plain, nil, false), "Reconcile confirming a non-candidate is not success")
	require.Equal(t, "indeterminate", subjectInstallResult(plain, revision.ErrIndeterminate, false))
	require.Equal(t, "indeterminate", subjectInstallResult(plain, context.DeadlineExceeded, false))

	require.Equal(t, "replay", subjectTransition(fastSandboxSubjectIntent{replay: true}))
	require.Equal(t, "replay", subjectTransition(fastSandboxSubjectIntent{replay: true, snapshot: &fastSandboxSubjectSnapshot{}}))
	require.Equal(t, "install", subjectTransition(fastSandboxSubjectIntent{snapshot: &fastSandboxSubjectSnapshot{}}))
	require.Equal(t, "unload", subjectTransition(fastSandboxSubjectIntent{unload: true}))
	require.Equal(t, "suspend", subjectTransition(suspend))
	require.Equal(t, "register", subjectTransition(register))
}

// Publication marks only the intents it rebuilds; later intents keep their
// own source, and install coalescing keeps the surviving intent's source.
func TestFastSandboxSubjectMetricsReplaySource(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	s.startSubjectDrain()
	// No worker: every intent stays queued for exact inspection.
	s.subjectDrain.cancel()
	<-s.subjectDrain.done
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	token, err := revision.NewSessionToken()
	require.NoError(t, err)
	client, err := revision.NewSubjectLifecycleClient("/nonexistent/receiver.sock", token, "control-metrics")
	require.NoError(t, err)
	s.publishSubjectClient(client, mitmproxy.SubjectRevisionIPCConfig{TLSAdmission: true})
	type source struct {
		kind   string
		replay bool
	}
	queued := func() []source {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []source
		for _, intent := range s.subjectDrain.queue {
			out = append(out, source{subjectTransition(intent), intent.replay})
		}
		return out
	}
	require.Equal(t, []source{{"replay", true}, {"replay", true}}, queued(), "rebuilt Register and install")
	s.mu.Lock()
	require.Nil(t, s.subjectDrain.queue[0].snapshot)
	require.NotNil(t, s.subjectDrain.queue[1].snapshot)
	s.mu.Unlock()
	// A normal committed snapshot replaces the queued replay install in place.
	s.policyCommitMu.Lock()
	s.recordBindingSnapshot(subject.FromSandboxUID(testUID), subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA})
	s.policyCommitMu.Unlock()
	require.Equal(t, []source{{"replay", true}, {"install", false}}, queued())
	// New actions after publication are normal transitions.
	require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	got := queued()
	require.Equal(t, []source{{"replay", true}, {"register", false}, {"install", false}, {"unload", false}}, got,
		"Unload drops the subject's queued install; the replay Register keeps its source")
	// A second publication rebuilds only the surviving set, again as replay.
	s.publishSubjectClient(client, mitmproxy.SubjectRevisionIPCConfig{TLSAdmission: true})
	require.Equal(t, []source{{"replay", true}, {"replay", true}}, queued())
}

func metricsSnapshotHarness(t *testing.T, outcome, operation string, deadline time.Duration) (*fastSandboxPolicyServer, *mitmproxy.SubjectRevisionIPCConfig, *snapshotFault) {
	t.Helper()
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	t.Cleanup(func() { o.stop(child) })
	h := (*configs)[0].SubjectRevisionIPC
	fault, proxy := startSnapshotFault(t, h, outcome, operation)
	proxy.TLSAdmission = true // observation gate only; the fixture receiver ignores it
	client, err := revision.NewSubjectLifecycleClient(proxy.SocketPath, proxy.SessionToken, proxy.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	s.mu.Lock()
	s.subjectDrain.deadline = deadline
	s.mu.Unlock()
	s.publishSubjectClient(client, proxy)
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
	return s, h, fault
}

func awaitMetricsFault(t *testing.T, ch <-chan struct{}, phase string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s not reached", phase)
	}
}

func TestFastSandboxSubjectMetricsInstallClassification(t *testing.T) {
	target := func(t *testing.T) revision.SubjectTarget { return lifecycleTarget(t, testUID, testFenceR, testFenceA) }
	bind := func(t *testing.T, s *fastSandboxPolicyServer) {
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	}
	t.Run("confirmed-rejection", func(t *testing.T) {
		export := captureTransitions(t)
		s, h, fault := metricsSnapshotHarness(t, "subject_failed", "prepare", 100*time.Millisecond)
		bind(t, s)
		awaitMetricsFault(t, fault.reached, "rejection")
		require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
		waitSubjectInstalled(t, h, lifecycleTarget(t, "other", "r2", "a2"), latestSubjectSnapshot(t, s, "other"))
		export.waitTransitions(map[string]int64{"register/ok": 2, "install/rejected": 1, "install/ok": 1}, "subject_failed")
		require.Equal(t, []string{"subject_failed"}, s.subjectLifecycleDegraded())
	})
	t.Run("lost-commit-then-readback", func(t *testing.T) {
		export := captureTransitions(t)
		s, h, fault := metricsSnapshotHarness(t, "", "", 100*time.Millisecond)
		bind(t, s)
		awaitMetricsFault(t, fault.reached, "Commit")
		// Apply and each blocked Reconcile are separate observations.
		export.waitFor(func(c map[string]int64) bool { return c["install/indeterminate"] >= 2 })
		close(fault.release)
		waitSubjectInstalled(t, h, target(t), latestSubjectSnapshot(t, s, testUID))
		counts := export.waitFor(func(c map[string]int64) bool { return c["install/ok"] == 1 })
		require.Equal(t, int64(1), counts["register/ok"])
		require.GreaterOrEqual(t, counts["install/indeterminate"], int64(2))
		require.Len(t, counts, 3, "%v", counts)
	})
	t.Run("acknowledged-abort", func(t *testing.T) {
		// Prepare loses its connection; Abort on the same call is confirmed:
		// one failed Apply (non-install), then a fresh Apply installs.
		export := captureTransitions(t)
		s, h, fault := metricsSnapshotHarness(t, "", "prepare-disconnect", 2*time.Second)
		bind(t, s)
		awaitMetricsFault(t, fault.abortReached, "Abort")
		close(fault.release)
		waitSubjectInstalled(t, h, target(t), latestSubjectSnapshot(t, s, testUID))
		export.waitTransitions(map[string]int64{"register/ok": 1, "install/failed": 1, "install/ok": 1})
	})
	t.Run("reconciled-abort", func(t *testing.T) {
		// Prepare times out and its Abort cannot finish: indeterminate. A later
		// Reconcile confirms Abort without the candidate: failed, not ok.
		export := captureTransitions(t)
		s, h, fault := metricsSnapshotHarness(t, "", "prepare-timeout", 100*time.Millisecond)
		bind(t, s)
		awaitMetricsFault(t, fault.abortReached, "Abort")
		export.waitFor(func(c map[string]int64) bool { return c["install/indeterminate"] >= 1 })
		close(fault.release)
		awaitMetricsFault(t, fault.retryReached, "successor Prepare")
		export.waitFor(func(c map[string]int64) bool { return c["install/failed"] == 1 })
		close(fault.retryRelease)
		waitSubjectInstalled(t, h, target(t), latestSubjectSnapshot(t, s, testUID))
		counts := export.waitFor(func(c map[string]int64) bool { return c["install/ok"] == 1 })
		require.Equal(t, int64(1), counts["register/ok"])
		require.Equal(t, int64(1), counts["install/failed"])
		require.GreaterOrEqual(t, counts["install/indeterminate"], int64(1))
		require.Len(t, counts, 4, "%v", counts)
	})
}

func TestFastSandboxSubjectMetricsLifecycleClassification(t *testing.T) {
	t.Run("confirmed-rejection", func(t *testing.T) {
		export := captureTransitions(t)
		s, _, _ := fastSandboxTestServer(t)
		o := subjectTestOwner(t)
		o.cfg.tlsAdmission = true
		launch, configs := subjectAddonLauncher(t, o)
		bounded := func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
			h := *cfg.SubjectRevisionIPC
			h.MaxSubjects = 1
			cfg.SubjectRevisionIPC = &h
			return launch(cfg)
		}
		child, err := o.launch(context.Background(), mitmproxy.Config{}, bounded)
		require.NoError(t, err)
		defer o.stop(child)
		s.startSubjectDrain()
		defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
		s.publishSubjectClient(o.sessions[child].client, o.sessions[child].handoff)
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
		require.Equal(t, 200, doAction(t, s, bindBody(t, "overflow", "10.0.0.9", "r2", "a2", 1, nil)).Code)
		waitLifecycleIdle(t, s, (*configs)[0].SubjectRevisionIPC, 2)
		// The accepted subject also installs its (empty) committed snapshot.
		export.waitTransitions(map[string]int64{"register/ok": 1, "register/rejected": 1, "install/ok": 1}, "subject_capacity_exhausted", "overflow")
	})
	t.Run("stale-intent-not-observed", func(t *testing.T) {
		// Intents dropped before send are not executed and record nothing.
		export := captureTransitions(t)
		s, _, _ := fastSandboxTestServer(t)
		o := subjectTestOwner(t)
		o.cfg.tlsAdmission = true
		launch, configs := subjectAddonLauncher(t, o)
		child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
		require.NoError(t, err)
		defer o.stop(child)
		s.startSubjectDrain()
		defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
		s.publishSubjectClient(o.sessions[child].client, o.sessions[child].handoff)
		h := (*configs)[0].SubjectRevisionIPC
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
		waitLifecycleIdle(t, s, h, 1)
		want := map[string]int64{"register/ok": 1, "install/ok": 1}
		export.waitTransitions(want)
		s.mu.Lock()
		subj := subject.FromSandboxUID(testUID)
		stale := fastSandboxSubjectIntent{subj: subj, fence: subject.Fencing{RuntimeInstanceID: "old", AttachmentID: "old"}, target: lifecycleTarget(t, testUID, "old", "old")}
		unload := fastSandboxSubjectIntent{subj: subj, fence: subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}, target: lifecycleTarget(t, testUID, testFenceR, testFenceA), unload: true}
		s.subjectDrain.queue = append(s.subjectDrain.queue, stale, unload)
		s.subjectDrain.signal()
		s.mu.Unlock()
		waitLifecycleIdle(t, s, h, 1)
		export.waitTransitions(want)
	})
	t.Run("pending-then-confirmed", func(t *testing.T) {
		// The Register reply is lost and status is held: the call and each
		// Reconcile are indeterminate; the Reconcile that confirms is ok.
		export := captureTransitions(t)
		s, _, _ := fastSandboxTestServer(t)
		o := subjectTestOwner(t)
		launch, configs := subjectAddonLauncher(t, o)
		child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
		require.NoError(t, err)
		defer o.stop(child)
		h := (*configs)[0].SubjectRevisionIPC
		fault, path := startLifecycleFaultProxy(t, h, false)
		client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
		require.NoError(t, err)
		s.startSubjectDrain()
		defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
		s.mu.Lock()
		s.subjectDrain.deadline = 100 * time.Millisecond
		s.mu.Unlock()
		handoff := *h
		handoff.SocketPath = path
		handoff.TLSAdmission = true
		s.publishSubjectClient(client, handoff)
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
		awaitMetricsFault(t, fault.consumed, "Register")
		export.waitFor(func(c map[string]int64) bool { return c["register/indeterminate"] >= 2 })
		close(fault.release)
		waitLifecycleIdle(t, s, h, 1)
		counts := export.waitFor(func(c map[string]int64) bool { return c["register/ok"] == 1 && c["install/ok"] == 1 })
		require.Len(t, counts, 3, "no rejected or failed series: %v", counts)
		fault.mu.Lock()
		require.Len(t, fault.commands, 1, "observation must not resend the consumed command")
		fault.mu.Unlock()
	})
	for _, mode := range []string{"shutdown", "replacement"} {
		t.Run(mode+"-is-not-observed", func(t *testing.T) {
			// A cancelled in-flight command is neither a receiver rejection
			// nor a failure: it records nothing.
			export := captureTransitions(t)
			s, _, _ := fastSandboxTestServer(t)
			o := subjectTestOwner(t)
			o.cfg.tlsAdmission = true
			launch, configs := subjectAddonLauncher(t, o)
			child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
			require.NoError(t, err)
			defer o.stop(child)
			h := (*configs)[0].SubjectRevisionIPC
			fault, path := startLifecycleFaultProxy(t, h, true)
			client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
			require.NoError(t, err)
			s.startSubjectDrain()
			s.mu.Lock()
			s.subjectDrain.deadline = time.Minute // only cancellation ends the held command
			s.mu.Unlock()
			handoff := *h
			handoff.SocketPath = path
			s.publishSubjectClient(client, handoff)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
			awaitMetricsFault(t, fault.consumed, "Register")
			if mode == "shutdown" {
				s.subjectDrain.cancel()
				<-s.subjectDrain.done
				require.Empty(t, export.collect())
				return
			}
			defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
			next, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
			require.NoError(t, err)
			defer o.stop(next)
			s.publishSubjectClient(o.sessions[next].client, o.sessions[next].handoff)
			nextH := (*configs)[1].SubjectRevisionIPC
			waitSubjectKnown(t, nextH, lifecycleTarget(t, testUID, testFenceR, testFenceA), true)
			waitLifecycleIdle(t, s, nextH, 1)
			waitSubjectInstalled(t, nextH, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
			// Only the new child's replayed Register and install are observed.
			export.waitTransitions(map[string]int64{"replay/ok": 2})
		})
	}
}
