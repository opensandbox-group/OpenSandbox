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

package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func prepareMappedMemory(t *testing.T) {
	t.Helper()
	mappedMemoryMu.Lock()
	previousMemory := mappedValue
	mappedValue = make([]byte, mappedMemorySize)
	mappedMemoryMu.Unlock()
	t.Cleanup(func() {
		mappedMemoryMu.Lock()
		mappedValue = previousMemory
		mappedMemoryMu.Unlock()
	})
}

func TestGuestHTTPHandlerSetGetAndStatus(t *testing.T) {
	prepareMappedMemory(t)
	handler := newGuestHTTPHandler()

	setResponse := httptest.NewRecorder()
	handler.ServeHTTP(setResponse, httptest.NewRequest(http.MethodPut, "/value", strings.NewReader("memory-survives")))
	if setResponse.Code != http.StatusOK {
		t.Fatalf("PUT /value status=%d body=%s", setResponse.Code, setResponse.Body.String())
	}

	mappedMemoryMu.Lock()
	binary.LittleEndian.PutUint64(mappedValue[mappedMemorySize-8:], 43)
	mappedMemoryMu.Unlock()

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/status", nil))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("GET /status status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var status memoryStatus
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Value != "memory-survives" || status.Counter != 43 {
		t.Fatalf("unexpected status: %#v", status)
	}

	valueResponse := httptest.NewRecorder()
	handler.ServeHTTP(valueResponse, httptest.NewRequest(http.MethodGet, "/value", nil))
	if valueResponse.Code != http.StatusOK || !strings.Contains(valueResponse.Body.String(), "memory-survives") {
		t.Fatalf("GET /value status=%d body=%s", valueResponse.Code, valueResponse.Body.String())
	}
}

func TestGuestHTTPHandlerRejectsOversizedValue(t *testing.T) {
	prepareMappedMemory(t)
	response := httptest.NewRecorder()
	newGuestHTTPHandler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPut, "/value", strings.NewReader(strings.Repeat("x", mappedValueCapacity+1))),
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", response.Code)
	}
}
