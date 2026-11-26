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

package suite

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// WakeStormOption configures optional hooks and timings for WakeStormSuite.
type WakeStormOption func(*WakeStormSuite)

// WithWakePollInterval overrides the polling interval while waiting for
// sandboxes to auto-pause and become ready.
func WithWakePollInterval(d time.Duration) WakeStormOption {
	return func(s *WakeStormSuite) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// WithWakeResumeCounter injects a white-box counter returning the cumulative
// FastPath ResumeSandbox RPC count so singleflight_coalesce_ratio can be
// computed directly against control-plane resumes (OSEP-0034 §4.4).
func WithWakeResumeCounter(fn func(ctx context.Context) (int64, error)) WakeStormOption {
	return func(s *WakeStormSuite) {
		s.resumeCounter = fn
	}
}

// WithWakeRetryCap bounds the maximum sleep honored from a Retry-After header.
func WithWakeRetryCap(d time.Duration) WakeStormOption {
	return func(s *WakeStormSuite) {
		if d > 0 {
			s.retryCap = d
		}
	}
}

// WithWakeMaxRetries sets the maximum number of wake retries per request on
// 503 + Retry-After before giving up.
func WithWakeMaxRetries(n int) WakeStormOption {
	return func(s *WakeStormSuite) {
		if n >= 0 {
			s.maxRetries = n
		}
	}
}

// WakeStormSuite benchmarks OSEP-0024 auto-pause and ingress wake-on-access
// under concurrent fan-out and singleflight bursts (OSEP-0034 §3, §4.4).
type WakeStormSuite struct {
	pollInterval  time.Duration
	resumeCounter func(ctx context.Context) (int64, error)
	retryCap      time.Duration
	maxRetries    int
}

// NewWakeStormSuite constructs the Phase 1 wake-storm suite.
func NewWakeStormSuite(opts ...WakeStormOption) *WakeStormSuite {
	s := &WakeStormSuite{
		pollInterval: 250 * time.Millisecond,
		retryCap:     60 * time.Second,
		maxRetries:   opensandbox.DefaultWakeMaxAttempts,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *WakeStormSuite) Name() string { return "wake-storm" }

func (s *WakeStormSuite) Description() string {
	return "OSEP-0024 auto-pause + concurrent ingress fan-out and singleflight wake bursts"
}

func (s *WakeStormSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

type wakeFleetMember struct {
	id        string
	epURL     string
	epHeaders map[string]string
}

type wakeProbeResult struct {
	sandboxID        string
	wakeToResponseMs float64
	retryConvergeMs  float64
	retryCount       int
	parkTimeouts     int
	sheds            int
	flightID         string
	status           string
	errCode          string
}

func (s *WakeStormSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()
	spec := &profile.WakeStormSuiteSpec{
		FleetSize:            4,
		IdleThresholdSeconds: opensandbox.IdleTimeoutMinSeconds,
		FanoutConcurrency:    4,
		SingleflightBurst:    8,
	}
	if rc.Profile != nil && rc.Profile.Suites.WakeStorm != nil {
		spec = rc.Profile.Suites.WakeStorm
	}
	fleetSize := spec.FleetSize
	if fleetSize <= 0 {
		fleetSize = 4
	}
	idleSec := spec.IdleThresholdSeconds
	if idleSec < opensandbox.IdleTimeoutMinSeconds {
		idleSec = opensandbox.IdleTimeoutMinSeconds
	}
	fanoutConc := spec.FanoutConcurrency
	if fanoutConc <= 0 {
		fanoutConc = fleetSize
	}
	if fanoutConc > fleetSize {
		fanoutConc = fleetSize
	}
	sfBurst := spec.SingleflightBurst
	if sfBurst <= 0 {
		sfBurst = 8
	}

	client := newScaleClient(rc)
	rec := newScaleRecorder()

	// 1. Create fleet with OSEP-0024 lifecycle.idlePolicy.
	wakeOnAccess := true
	idlePolicy := &opensandbox.IdlePolicy{
		IdleTimeoutSeconds: &idleSec,
		OnIdle:             opensandbox.OnIdlePause,
		WakeOnAccess:       &wakeOnAccess,
	}
	lifecycle := &opensandbox.SandboxLifecycle{
		IdlePolicy: idlePolicy,
	}

	fleet := make([]wakeFleetMember, 0, fleetSize)
	defer func() {
		var wg sync.WaitGroup
		for _, m := range fleet {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				client.deleteSandboxBestEffort(id)
			}(m.id)
		}
		wg.Wait()
	}()

	var fleetMu sync.Mutex
	var createWG sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := 0; i < fleetSize; i++ {
		createWG.Add(1)
		go func() {
			defer createWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			req := scaleBuildBaseCreateRequest(rc.Profile, scaleDefaultTimeoutSec, lifecycle)
			info, err := client.lifecycle.CreateSandbox(ctx, req)
			if err != nil {
				rec.recordOp(OperationSample{
					Operation: "wake_fleet_create",
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			readyCtx, cancel := context.WithTimeout(ctx, scaleDefaultReadyTimeout)
			epURL, epHeaders, err := client.waitSandboxRunningAndPing(readyCtx, info.ID, s.pollInterval)
			cancel()
			if err != nil {
				client.deleteSandboxBestEffort(info.ID)
				rec.recordOp(OperationSample{
					Operation: "wake_fleet_ready",
					SandboxID: info.ID,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			fleetMu.Lock()
			fleet = append(fleet, wakeFleetMember{
				id:        info.ID,
				epURL:     epURL,
				epHeaders: epHeaders,
			})
			fleetMu.Unlock()
		}()
	}
	createWG.Wait()

	if len(fleet) == 0 {
		return &report.SuiteResult{
			Name:            s.Name(),
			Status:          "failed",
			StartedAt:       startedAt,
			DurationSeconds: time.Since(startedAt).Seconds(),
			TotalOps:        rec.totalOps,
			SucceededOps:    rec.succeededOps,
			FailedOps:       rec.failedOps,
			Errors:          rec.errors,
		}, rec.samples, nil
	}

	// 2. Wait for all fleet sandboxes to auto-pause.
	pauseTimeout := time.Duration(idleSec)*time.Second + scaleDefaultPauseTimeout
	pausedFleet := make([]wakeFleetMember, 0, len(fleet))
	var pauseMu sync.Mutex
	var pauseWG sync.WaitGroup
	for _, m := range fleet {
		pauseWG.Add(1)
		go func(member wakeFleetMember) {
			defer pauseWG.Done()
			pCtx, cancel := context.WithTimeout(ctx, pauseTimeout)
			_, err := client.waitSandboxState(pCtx, member.id, opensandbox.StatePaused, s.pollInterval)
			cancel()
			if err != nil {
				rec.recordOp(OperationSample{
					Operation: "wake_wait_autopause",
					SandboxID: member.id,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			pauseMu.Lock()
			pausedFleet = append(pausedFleet, member)
			pauseMu.Unlock()
		}(m)
	}
	pauseWG.Wait()

	if len(pausedFleet) == 0 {
		return &report.SuiteResult{
			Name:            s.Name(),
			Status:          "failed",
			StartedAt:       startedAt,
			DurationSeconds: time.Since(startedAt).Seconds(),
			TotalOps:        rec.totalOps,
			SucceededOps:    rec.succeededOps,
			FailedOps:       rec.failedOps,
			Errors:          rec.errors,
		}, rec.samples, nil
	}

	var totalWakeReqs int
	var reqsWithParkTimeout int
	var totalParkTimeouts int
	var totalSheds int
	var totalRetries int
	var maxRetriesObserved int

	recordWakeResult := func(opName, extraKey string, r wakeProbeResult) {
		totalWakeReqs++
		if r.parkTimeouts > 0 {
			reqsWithParkTimeout++
		}
		totalParkTimeouts += r.parkTimeouts
		totalSheds += r.sheds
		totalRetries += r.retryCount
		if r.retryCount > maxRetriesObserved {
			maxRetriesObserved = r.retryCount
		}
		if r.status == "ok" {
			rec.addLatency("wake_to_response_ms", r.wakeToResponseMs)
			rec.addLatency("t_first_ping", r.wakeToResponseMs)
			if extraKey != "" {
				rec.addLatency(extraKey, r.wakeToResponseMs)
			}
			if r.retryCount > 0 {
				rec.addLatency("sdk_retry_convergence_ms", r.retryConvergeMs)
			}
		}
		rec.recordOp(OperationSample{
			Operation: opName,
			SandboxID: r.sandboxID,
			LatencyMs: r.wakeToResponseMs,
			Status:    r.status,
			ErrorCode: r.errCode,
		})
	}

	// 3. (a) Fan-out burst: 1 concurrent ingress request per paused sandbox.
	fanoutCount := fanoutConc
	if fanoutCount > len(pausedFleet) {
		fanoutCount = len(pausedFleet)
	}
	fanoutResults := make([]wakeProbeResult, fanoutCount)
	fanoutStart := make(chan struct{})
	var fanoutWG sync.WaitGroup
	for i := 0; i < fanoutCount; i++ {
		fanoutWG.Add(1)
		go func(idx int, member wakeFleetMember) {
			defer fanoutWG.Done()
			<-fanoutStart
			fanoutResults[idx] = s.fireWakeRequest(ctx, client, member)
		}(i, pausedFleet[i])
	}
	close(fanoutStart)
	fanoutWG.Wait()

	for _, r := range fanoutResults {
		recordWakeResult("wake_fanout", "fanout_wake_to_response_ms", r)
	}

	// 4. (b) Singleflight burst: N concurrent requests to 1 paused sandbox.
	var sfTarget wakeFleetMember
	var sfPauseErr error
	if len(pausedFleet) > fanoutCount {
		sfTarget = pausedFleet[fanoutCount]
	} else {
		// Re-pause the first sandbox so singleflight targets a confirmed Paused sandbox.
		sfTarget = pausedFleet[0]
		if err := client.lifecycle.PauseSandbox(ctx, sfTarget.id); err != nil {
			sfPauseErr = fmt.Errorf("re-pause %s before singleflight: %w", sfTarget.id, err)
		} else {
			pCtx, cancel := context.WithTimeout(ctx, scaleDefaultPauseTimeout)
			_, err = client.waitSandboxState(pCtx, sfTarget.id, opensandbox.StatePaused, s.pollInterval)
			cancel()
			if err != nil {
				sfPauseErr = fmt.Errorf("wait re-pause %s before singleflight: %w", sfTarget.id, err)
			}
		}
	}

	var coalesceRatio float64
	coalesceSource := "unmeasured"

	if sfPauseErr != nil {
		rec.recordOp(OperationSample{
			Operation: "wake_singleflight_repause",
			SandboxID: sfTarget.id,
			Status:    "error",
			ErrorCode: scaleErrorCode(sfPauseErr),
		})
	} else {
		resumeCounter := s.resumeCounter
		if resumeCounter == nil {
			resumeCounter = BenchHooksFromContext(ctx).WakeResumeCounter
		}

		var resumeBefore int64
		var resumeBeforeOK bool
		if resumeCounter != nil {
			if v, err := resumeCounter(ctx); err == nil {
				resumeBefore = v
				resumeBeforeOK = true
			}
		}

		sfResults := make([]wakeProbeResult, sfBurst)
		sfStart := make(chan struct{})
		var sfWG sync.WaitGroup
		for i := 0; i < sfBurst; i++ {
			sfWG.Add(1)
			go func(idx int) {
				defer sfWG.Done()
				<-sfStart
				sfResults[idx] = s.fireWakeRequest(ctx, client, sfTarget)
			}(i)
		}
		close(sfStart)
		sfWG.Wait()

		uniqueFlights := make(map[string]struct{})
		for _, r := range sfResults {
			if r.flightID != "" {
				uniqueFlights[r.flightID] = struct{}{}
			}
			recordWakeResult("wake_singleflight", "singleflight_wake_to_response_ms", r)
		}

		if resumeBeforeOK && resumeCounter != nil {
			if resumeAfter, err := resumeCounter(ctx); err == nil {
				delta := resumeAfter - resumeBefore
				if delta > 0 {
					coalesceRatio = float64(sfBurst) / float64(delta)
					coalesceSource = "fastpath_resume_counter"
				}
			}
		}
		if coalesceSource == "unmeasured" && len(uniqueFlights) > 0 {
			coalesceRatio = float64(sfBurst) / float64(len(uniqueFlights))
			coalesceSource = "flight_header"
		}
	}

	var parkTimeoutRate float64
	var shedRate float64
	var avgRetries float64
	if totalWakeReqs > 0 {
		// Rates are the fraction of logical wake requests that experienced a park timeout or shed.
		parkTimeoutRate = float64(reqsWithParkTimeout) / float64(totalWakeReqs)
		shedRate = float64(totalSheds) / float64(totalWakeReqs)
		avgRetries = float64(totalRetries) / float64(totalWakeReqs)
	}

	durationSec := time.Since(startedAt).Seconds()
	latencies := make(map[string]report.LatencyDist, len(rec.latencies))
	for k, vals := range rec.latencies {
		if len(vals) > 0 {
			latencies[k] = report.ComputeLatencyDist(vals)
		}
	}

	status := "passed"
	if rec.totalOps == 0 || rec.failedOps > 0 {
		status = "failed"
	}
	var throughput float64
	if durationSec > 0 && rec.succeededOps > 0 {
		throughput = float64(rec.succeededOps) / durationSec
	}

	res := &report.SuiteResult{
		Name:            s.Name(),
		Status:          status,
		StartedAt:       startedAt,
		DurationSeconds: durationSec,
		ThroughputRPS:   throughput,
		TotalOps:        rec.totalOps,
		SucceededOps:    rec.succeededOps,
		FailedOps:       rec.failedOps,
		Errors:          rec.errors,
		Latencies:       latencies,
		Extra: map[string]any{
			"fleet_size":                  len(pausedFleet),
			"fanout_concurrency":          fanoutCount,
			"singleflight_burst":          sfBurst,
			"singleflight_coalesce_ratio": coalesceRatio,
			"singleflight_source":         coalesceSource,
			"wake_park_timeout_rate":      parkTimeoutRate,
			"wake_shed_rate":              shedRate,
			"wake_park_timeout_count":     totalParkTimeouts,
			"wake_shed_count":             totalSheds,
			"total_wake_requests":         totalWakeReqs,
			"retry_count_total":           totalRetries,
			"retry_count_avg":             avgRetries,
			"retry_count_max":             maxRetriesObserved,
		},
	}
	return res, rec.samples, nil
}

// fireWakeRequest sends GET /ping through the sandbox's ingress endpoint and
// retries on OSEP-0024 503 + Retry-After responses until 200 or budget exhaustion.
func (s *WakeStormSuite) fireWakeRequest(ctx context.Context, client *scaleClient, member wakeFleetMember) wakeProbeResult {
	res := wakeProbeResult{
		sandboxID: member.id,
		status:    "error",
	}
	pingURL := strings.TrimRight(member.epURL, "/") + "/ping"
	start := time.Now()
	var firstRetryAt time.Time

	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		statusCode, hdr, _, err := client.probeURL(ctx, pingURL, member.epHeaders)
		if hdr != nil {
			if fid := strings.TrimSpace(hdr.Get("OpenSandbox-Wake-Flight")); fid != "" {
				res.flightID = fid
			} else if fid := strings.TrimSpace(hdr.Get("X-OpenSandbox-Wake-Flight")); fid != "" {
				res.flightID = fid
			}
		}
		if err != nil {
			res.wakeToResponseMs = scaleElapsedMs(start)
			res.errCode = scaleErrorCode(err)
			return res
		}
		if statusCode == http.StatusOK {
			res.wakeToResponseMs = scaleElapsedMs(start)
			if res.retryCount > 0 && !firstRetryAt.IsZero() {
				res.retryConvergeMs = scaleElapsedMs(firstRetryAt)
			}
			res.status = "ok"
			return res
		}
		if statusCode == http.StatusServiceUnavailable {
			hasRetryAfter := hdr != nil && strings.TrimSpace(hdr.Get("Retry-After")) != ""
			isWakeExpired := hdr != nil && strings.EqualFold(strings.TrimSpace(hdr.Get("OpenSandbox-Wake")), "expired")
			if isWakeExpired || hasRetryAfter {
				res.parkTimeouts++
				if firstRetryAt.IsZero() {
					firstRetryAt = time.Now()
				}
				if attempt == s.maxRetries {
					res.wakeToResponseMs = scaleElapsedMs(start)
					res.errCode = "wake_park_timeout"
					return res
				}
				wait := scaleParseRetryAfter(hdr, 100*time.Millisecond, s.retryCap)
				res.retryCount++
				select {
				case <-ctx.Done():
					res.wakeToResponseMs = scaleElapsedMs(start)
					res.errCode = scaleErrorCode(ctx.Err())
					return res
				case <-time.After(wait):
				}
				continue
			}
			// Bare 503 without Retry-After / OpenSandbox-Wake: expired indicates
			// pre-resume shedding (--wake-park-max / --wake-restore-max).
			res.sheds++
			res.wakeToResponseMs = scaleElapsedMs(start)
			res.errCode = "wake_shed_503"
			return res
		}

		res.wakeToResponseMs = scaleElapsedMs(start)
		res.errCode = fmt.Sprintf("http_%d", statusCode)
		return res
	}

	res.wakeToResponseMs = scaleElapsedMs(start)
	res.errCode = "wake_retry_exhausted"
	return res
}
