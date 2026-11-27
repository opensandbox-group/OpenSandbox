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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteIngressError(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			writer := httptest.NewRecorder()
			writeIngressError(writer, errors.New("route unavailable"), status)
			if writer.Code != status || writer.Body.String() != "OpenSandbox Ingress: route unavailable\n" {
				t.Fatalf("response = %d %q", writer.Code, writer.Body.String())
			}
			wantRetry := ""
			if status == http.StatusServiceUnavailable {
				wantRetry = "1"
			}
			if got := writer.Header().Get("Retry-After"); got != wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, wantRetry)
			}
		})
	}
}
