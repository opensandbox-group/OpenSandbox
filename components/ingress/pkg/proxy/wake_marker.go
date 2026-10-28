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

package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
)

// WakeMarkerHeader is gateway-owned: the proxy strips it from upstream
// responses, so its presence proves the gateway never forwarded the
// request. Post-forward answers stay bare.
var WakeMarkerHeader = http.CanonicalHeaderKey("OpenSandbox-Wake")

const (
	WakeMarkerExpired  = "expired"
	WakeTimeoutCode    = "sandbox_wake_timeout"
	WakeTimeoutMessage = "restore still in progress; request was never forwarded and is safe to retry"
)

// wakeGateError wraps pre-forward parking errors with the wake target; its
// presence is what keeps the marker off post-forward answers.
type wakeGateError struct {
	target sandbox.EndpointTarget
	err    error
}

func (e *wakeGateError) Error() string { return e.err.Error() }

func (e *wakeGateError) Unwrap() error { return e.err }

type wakeTimeoutBody struct {
	Error wakeTimeoutErrorBody `json:"error"`
}

type wakeTimeoutErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SandboxID string `json:"sandboxId,omitempty"`
}

// respondWakeTimeout writes the marked wake-timeout 503 for a pre-forward
// park-budget exhaustion; reports whether it did.
func respondWakeTimeout(w http.ResponseWriter, err error) bool {
	var gate *wakeGateError
	if !errors.As(err, &gate) || !errors.Is(err, wake.ErrBudgetExhausted) {
		return false
	}
	encoded, marshalErr := json.Marshal(wakeTimeoutBody{Error: wakeTimeoutErrorBody{
		Code:      WakeTimeoutCode,
		Message:   WakeTimeoutMessage,
		SandboxID: gate.target.SandboxID,
	}})
	if marshalErr != nil {
		return false
	}
	header := w.Header()
	header.Set("Retry-After", "1")
	header.Set(WakeMarkerHeader, WakeMarkerExpired)
	header.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(encoded)
	return true
}

// stripWakeMarkerObserver removes the gateway-owned marker from upstream
// responses. Trailers are filled after ModifyResponse (at body EOF), so the
// body is wrapped to delete the key again there — otherwise a sandbox could
// forge the marker in a trailer.
func stripWakeMarkerObserver() func(*http.Response) {
	return func(response *http.Response) {
		response.Header.Del(WakeMarkerHeader)
		if response.Trailer != nil {
			response.Trailer.Del(WakeMarkerHeader)
		}
		if response.Body != nil {
			response.Body = &trailerStripper{ReadCloser: response.Body, resp: response}
		}
	}
}

type trailerStripper struct {
	io.ReadCloser
	resp *http.Response
}

func (t *trailerStripper) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if err == io.EOF && t.resp.Trailer != nil {
		t.resp.Trailer.Del(WakeMarkerHeader)
	}
	return n, err
}
