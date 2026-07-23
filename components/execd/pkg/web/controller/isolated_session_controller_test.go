// Copyright 2026 Alibaba Group Holding Ltd.
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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestClassifyIsolatedCreateError_UidModeUnavailable(t *testing.T) {
	status, code := classifyIsolatedCreateError(
		fmt.Errorf("create session: %w: userns", runtime.ErrUidModeUnavailable),
	)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if code != model.ErrorCodeNotSupported {
		t.Errorf("code = %q, want %q", code, model.ErrorCodeNotSupported)
	}
}

func TestCapabilities_ReportsModeSpecificProbeResults(t *testing.T) {
	previousRunner := isolatedRunner
	previousProbe := isolatedProbeResult
	isolatedRunner = nil
	isolatedProbeResult = &isolation.ProbeResult{
		Available:        false,
		Isolator:         "bwrap",
		Version:          "0.11.0",
		Message:          "no uid mode available",
		SetprivAvailable: false,
		UsernsAvailable:  false,
	}
	t.Cleanup(func() {
		isolatedRunner = previousRunner
		isolatedProbeResult = previousProbe
	})

	ctx, recorder := newTestContext(http.MethodGet, "/v1/isolated/capabilities", nil)
	NewIsolatedSessionController(ctx).Capabilities()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response model.CapabilitiesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Isolator != "bwrap" || response.Version != "0.11.0" {
		t.Errorf("isolator/version = %q/%q", response.Isolator, response.Version)
	}
	if response.SetprivAvailable || response.UsernsAvailable {
		t.Errorf("uid modes should be unavailable: %+v", response)
	}

	var raw map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	for _, key := range []string{"setpriv_available", "userns_available"} {
		if value, ok := raw[key]; !ok || value != false {
			t.Errorf("%s = %#v, present=%v; want false and present", key, value, ok)
		}
	}
}
