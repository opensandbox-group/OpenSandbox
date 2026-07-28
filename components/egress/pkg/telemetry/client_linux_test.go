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

//go:build linux

package telemetry

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/sys/unix"
)

// The exporter client must carry SO_MARK: the sidecar shares a netns with the sandbox
// and is subject to the policy it installs, so unmarked exports are dropped.
func TestExporterHTTPClientSetsSoMark(t *testing.T) {
	client := exporterHTTPClient()
	if client == nil {
		t.Fatal("exporterHTTPClient() = nil on linux, want a client")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	if err != nil {
		// Setting SO_MARK needs CAP_NET_ADMIN; unprivileged CI cannot exercise the
		// happy path, but the failure must come from the mark and nothing else.
		if errors.Is(err, unix.EPERM) {
			t.Skip("SO_MARK requires CAP_NET_ADMIN; skipping in unprivileged environment")
		}
		t.Fatalf("request through marked client: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
