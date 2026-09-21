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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/ebpf"
	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/lifecycle"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// ErrAlreadyInitialized is returned when /internal/init is called after the
// one-shot init slot has been consumed.
var ErrAlreadyInitialized = errors.New("runtime init already accepted")

// maxInitTelemetryAttrs bounds the /internal/init telemetry attribute map.
const maxInitTelemetryAttrs = 64

// RuntimeInitConfig wires the manager to the process-wide collaborators it
// needs to apply a RuntimeBinding. Nil pointers disable the corresponding
// step (e.g. no isolated runner, classic mode has no supervised entrypoint).
type RuntimeInitConfig struct {
	Ctrl *runtime.Controller

	// IsolatedResetter clears the previous generation's isolated sessions
	// without shutting the runner down; nil when isolation is unavailable.
	IsolatedResetter interface{ Reset() error }

	// LaunchEntrypoint starts (or replaces) the supervised user entrypoint;
	// nil in classic mode where the container entrypoint is external.
	LaunchEntrypoint func([]string) error

	// EntrypointArgs are the user command arguments (init mode only).
	EntrypointArgs []string

	// TemplateLifecycle is the lifecycle config from the container template
	// (OPENSANDBOX_LIFECYCLE / persisted file). Used when /internal/init omits the
	// lifecycle field.
	TemplateLifecycle *lifecycle.Config

	// AppendStartupStatus reports lifecycle progress to the bootstrap
	// watchdog file (no-op when no status file is configured).
	AppendStartupStatus func(string) error
}

// RuntimeInitManager serializes POST /internal/init handling and owns the active
// periodic-hook manager. It also tracks readiness for GET /ready.
//
// /internal/init is strictly one-shot: the first VALID call consumes the init slot
// (accepted) regardless of whether the apply succeeds. Subsequent calls are
// rejected with 409 without waiting; a failed apply is not retried — the
// control plane recycles the container.
type RuntimeInitManager struct {
	cfg RuntimeInitConfig

	// accepted guards the one-shot slot. It is armed only after request
	// validation passes, so malformed calls never burn the slot.
	accepted atomic.Bool

	mu    sync.Mutex
	ready atomic.Bool

	periodic *lifecycle.PeriodicManager
}

var runtimeInitManager atomic.Pointer[RuntimeInitManager]

// InitRuntimeInitManager installs the process-wide manager. Call once from
// main after the runtime collaborators are constructed.
func InitRuntimeInitManager(cfg *RuntimeInitConfig) *RuntimeInitManager {
	manager := &RuntimeInitManager{cfg: *cfg}
	runtimeInitManager.Store(manager)
	return manager
}

// GetRuntimeInitManager returns the installed manager, or nil.
func GetRuntimeInitManager() *RuntimeInitManager {
	return runtimeInitManager.Load()
}

// SetPeriodic hands a legacy-startup periodic manager to the manager so
// shutdown and /internal/init swaps stop the right instance.
func (m *RuntimeInitManager) SetPeriodic(manager *lifecycle.PeriodicManager) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopPeriodicLocked()
	m.periodic = manager
}

// StopPeriodic stops the active periodic manager (process shutdown).
func (m *RuntimeInitManager) StopPeriodic() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopPeriodicLocked()
}

func (m *RuntimeInitManager) stopPeriodicLocked() {
	if m.periodic != nil {
		m.periodic.Stop()
		m.periodic = nil
	}
}

// MarkReady records that user workloads are allowed to run. The legacy
// startup path calls it after preStart + entrypoint; the /init path after
// applying the binding.
func (m *RuntimeInitManager) MarkReady() {
	if m == nil {
		return
	}
	m.ready.Store(true)
}

// Ready reports whether the apply sequence completed. The runtime-init gate
// consults this instead of binding presence: a half-completed apply (500)
// keeps a binding but must stay gated, matching the /ready 503.
func (m *RuntimeInitManager) Ready() bool {
	if m == nil {
		return false
	}
	return m.ready.Load()
}

// InitController serves POST /internal/init and GET /ready.
type InitController struct {
	*basicController
}

// NewInitController follows the per-request controller constructor pattern.
func NewInitController(ctx *gin.Context) *InitController {
	return &InitController{basicController: newBasicController(ctx)}
}

// Ready implements GET /ready: 200 once user workloads may run, 503 while
// execd is uninitialized or still starting up.
func (c *InitController) Ready() {
	initialized := GetRuntimeInitManager().Ready()
	status := http.StatusOK
	if !initialized {
		status = http.StatusServiceUnavailable
	}
	resp := model.RuntimeReadyResponse{Initialized: initialized}
	if b := binding.Current(); b != nil {
		resp.SandboxID = b.SandboxID
		resp.Generation = b.Generation
	}
	c.ctx.JSON(status, resp)
}

// Init implements POST /internal/init: validate, consume the one-shot init slot,
// apply the RuntimeBinding, run preStart, start the entrypoint, and mark
// execd ready.
func (c *InitController) Init() {
	manager := GetRuntimeInitManager()
	if manager == nil {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "runtime init is not available")
		return
	}

	var req model.RuntimeInitRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid runtime init request: "+err.Error())
		return
	}

	warnings, errorCode, httpStatus, err := manager.Apply(&req)
	if err != nil {
		c.RespondError(httpStatus, errorCode, err.Error())
		return
	}
	c.RespondSuccess(model.RuntimeInitResponse{
		Status:     "initialized",
		SandboxID:  req.SandboxID,
		Generation: req.Generation,
		Warning:    warnings,
	})
}

// Apply runs the one-shot /internal/init sequence. Status mapping: 400 invalid
// request (the slot is not consumed), 409 the init slot was already
// consumed by any earlier valid call, 500 startup failure after apply (no
// retry: the slot stays consumed).
func (m *RuntimeInitManager) Apply(req *model.RuntimeInitRequest) ([]string, model.ErrorCode, int, error) {
	warnings, err := validateInitRequest(req)
	if err != nil {
		return nil, model.ErrorCodeInvalidRequest, http.StatusBadRequest, err
	}
	var tokenHash [32]byte
	hasToken := req.AccessTokenHash != ""
	if hasToken {
		digest, err := binding.ParseAccessTokenHash(req.AccessTokenHash)
		if err != nil {
			return nil, model.ErrorCodeInvalidRequest, http.StatusBadRequest, err
		}
		tokenHash = digest
	}
	policy := req.EntrypointPolicy

	// Strictly once: the first valid call consumes the slot regardless of
	// the apply outcome. A concurrent or later caller is rejected without
	// waiting; the control plane reconciles via GET /ready.
	if !m.accepted.CompareAndSwap(false, true) {
		return nil, model.ErrorCodeAlreadyInitialized, http.StatusConflict,
			fmt.Errorf("%w; /internal/init is strictly one-shot (see GET /ready)", ErrAlreadyInitialized)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Stop any workloads started before init (legacy fallback path may
	// have run the template-driven startup already). With keep the
	// supervised entrypoint is spared and adopted as-is.
	m.stopPeriodicLocked()
	if m.cfg.Ctrl != nil {
		m.cfg.Ctrl.Reset()
	}
	if m.cfg.IsolatedResetter != nil {
		if err := m.cfg.IsolatedResetter.Reset(); err != nil {
			log.Warn("runtime init: isolated runner cleanup: %v", err)
			warnings = append(warnings, "isolated session cleanup reported errors")
		}
	}
	// Retire the supervised entrypoint BEFORE killing the remaining
	// children: retirement must be visible before the entrypoint process
	// exits, or its waiter treats the exit as the container exiting
	// (stopChildrenExcept + os.Exit) instead of a generation replacement.
	if policy == model.EntrypointPolicyRestart {
		runtime.RetireEntrypoint()
	}
	runtime.StopUserProcesses(policy == model.EntrypointPolicyKeep)

	// 2. Apply the RuntimeBinding atomically: auth, env resolution, and
	// telemetry attribution switch to the new sandbox in one swap.
	newBinding := &binding.RuntimeBinding{
		SandboxID:       req.SandboxID,
		Generation:      req.Generation,
		AccessTokenHash: tokenHash,
		HasAccessToken:  hasToken,
		Envs:            req.Envs,
	}
	if req.Telemetry != nil {
		newBinding.TelemetryAttrs = req.Telemetry.Attributes
	}
	binding.Apply(newBinding)
	log.Info("runtime init: binding applied sandbox_id=%s generation=%d", req.SandboxID, req.Generation)

	// Sandbox attribution for the eBPF observation layer (no-op for builds
	// without it).
	if state, msg := ebpf.SetSandboxID(req.SandboxID); state != "" {
		runtime.SetEbpfState(runtime.LayerState{State: state, Message: msg})
	}

	// 3. Run preStart, then start periodic hooks. An omitted lifecycle keeps
	// the template-level config (migration compatibility); a present one
	// (even empty) replaces it.
	lifecycleCfg := m.cfg.TemplateLifecycle
	if req.Lifecycle != nil {
		lifecycleCfg = req.Lifecycle
	}
	if err := m.runPreStart(lifecycleCfg); err != nil {
		return warnings, model.ErrorCodeRuntimeError, http.StatusInternalServerError, fmt.Errorf("runtime init preStart: %w", err)
	}
	periodicManager, err := lifecycle.StartPeriodic(lifecycleCfg)
	if err != nil {
		log.Error("runtime init: periodic hooks disabled: %v", err)
		warnings = append(warnings, "periodic hooks failed to start")
	}
	m.periodic = periodicManager

	// 4. Entrypoint policy. Default keep: never start or restart the user
	// entrypoint (a running one is adopted as-is). restart — init mode only
	// — retires the running entrypoint and starts a fresh one with the
	// RuntimeBinding env.
	switch {
	case policy == model.EntrypointPolicyRestart && m.cfg.LaunchEntrypoint != nil && len(m.cfg.EntrypointArgs) > 0:
		if err := m.cfg.LaunchEntrypoint(m.cfg.EntrypointArgs); err != nil {
			return warnings, model.ErrorCodeRuntimeError, http.StatusInternalServerError, fmt.Errorf("runtime init entrypoint: %w", err)
		}
	case policy == model.EntrypointPolicyRestart:
		if len(m.cfg.EntrypointArgs) > 0 {
			warnings = append(warnings, "entrypointPolicy=restart ignored: execd does not own the entrypoint in this mode")
		}
	case runtime.EntrypointRunning():
		log.Info("runtime init: keeping running entrypoint (entrypointPolicy=keep)")
	case len(m.cfg.EntrypointArgs) > 0:
		warnings = append(warnings, "entrypointPolicy=keep: template entrypoint was not started")
	}

	m.MarkReady()
	return warnings, "", http.StatusOK, nil
}

// runPreStart executes the lifecycle preStart hook, mirroring the legacy
// startup-status protocol ("running N" → "done 0|1") for the bootstrap
// watchdog. Status-file failures are logged, never fatal (bootstrap may
// already have consumed and removed the file); a preStart failure itself is
// returned so /internal/init reports 500 and stays uninitialized.
func (m *RuntimeInitManager) runPreStart(cfg *lifecycle.Config) error {
	if cfg != nil && cfg.PreStart != nil {
		m.appendStartupStatus(fmt.Sprintf("running %d", int64(cfg.PreStartTimeout()/time.Second)))
		if err := lifecycle.RunPreStart(context.Background(), cfg); err != nil {
			m.appendStartupStatus("done 1")
			return err
		}
	}
	m.appendStartupStatus("done 0")
	return nil
}

// appendStartupStatus reports lifecycle progress to the bootstrap watchdog
// file when configured.
func (m *RuntimeInitManager) appendStartupStatus(status string) {
	if m.cfg.AppendStartupStatus == nil {
		return
	}
	if err := m.cfg.AppendStartupStatus(status); err != nil {
		log.Warn("runtime init: append lifecycle startup status %q: %v", status, err)
	}
}

// validateInitRequest sanity-checks the /internal/init payload. Env keys
// colliding with execd's own config/credential names are rejected outright;
// reserved telemetry attribute keys are dropped with a warning.
func validateInitRequest(req *model.RuntimeInitRequest) ([]string, error) {
	if err := validateInitIdentity(req); err != nil {
		return nil, err
	}
	if err := validateInitEnvs(req.Envs); err != nil {
		return nil, err
	}
	if req.Lifecycle != nil {
		if err := lifecycle.ValidateConfig(req.Lifecycle); err != nil {
			return nil, fmt.Errorf("invalid lifecycle: %w", err)
		}
	}
	return validateInitTelemetry(req.Telemetry)
}

func validateInitIdentity(req *model.RuntimeInitRequest) error {
	req.SandboxID = strings.TrimSpace(req.SandboxID)
	if req.SandboxID == "" {
		return errors.New("sandboxId must not be blank")
	}
	if len(req.SandboxID) > 128 {
		return errors.New("sandboxId must not exceed 128 characters")
	}
	if req.Generation == 0 {
		return errors.New("generation must be a positive integer")
	}
	switch req.EntrypointPolicy {
	case "":
		// Default: never start or restart the entrypoint.
		req.EntrypointPolicy = model.EntrypointPolicyKeep
	case model.EntrypointPolicyKeep, model.EntrypointPolicyRestart:
	default:
		return fmt.Errorf("entrypointPolicy must be %q or %q", model.EntrypointPolicyKeep, model.EntrypointPolicyRestart)
	}
	return nil
}

func validateInitEnvs(envs map[string]string) error {
	if len(envs) == 0 {
		return nil
	}
	blocked := make(map[string]struct{})
	for _, name := range isolation.ExecdConfigEnvBlacklist() {
		blocked[strings.ToUpper(name)] = struct{}{}
	}
	for key := range envs {
		if key == "" {
			return errors.New("envs must not contain blank keys")
		}
		if len(key) > 256 {
			return fmt.Errorf("env key %q must not exceed 256 characters", key)
		}
		if _, found := blocked[strings.ToUpper(key)]; found {
			return fmt.Errorf("env key %q is reserved by execd and must not be set via /internal/init", key)
		}
	}
	return nil
}

func validateInitTelemetry(telemetry *model.RuntimeInitTelemetry) ([]string, error) {
	if telemetry == nil {
		return nil, nil
	}
	if len(telemetry.Attributes) > maxInitTelemetryAttrs {
		return nil, fmt.Errorf("telemetry attributes must not exceed %d entries", maxInitTelemetryAttrs)
	}
	var warnings []string
	for key, value := range telemetry.Attributes {
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("telemetry attributes must not contain blank keys")
		}
		if len(key) > 128 {
			return nil, fmt.Errorf("telemetry attribute key %q must not exceed 128 characters", key)
		}
		if len(value) > 1024 {
			return nil, fmt.Errorf("telemetry attribute %q value must not exceed 1024 bytes", key)
		}
		if key == "sandbox_id" || key == "generation" {
			// Delivered structurally from the binding; drop user attempts
			// to override them.
			delete(telemetry.Attributes, key)
			warnings = append(warnings, "telemetry attribute "+key+" is reserved and was dropped")
		}
	}
	return warnings, nil
}
