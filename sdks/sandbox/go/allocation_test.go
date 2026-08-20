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

package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSandboxInfoAllocationJSON(t *testing.T) {
	var info SandboxInfo
	err := json.Unmarshal([]byte(`{
		"id":"sbx-pooled",
		"status":{"state":"Running"},
		"createdAt":"2026-07-10T00:00:00Z",
		"allocation":{"mode":"pool","poolRef":"pool-runc","state":"allocated"}
	}`), &info)
	require.NoErrorf(t, err, "unmarshal sandbox allocation")
	require.NotNil(t, info.Allocation, "allocation should be present")
	require.Equal(t, AllocationModePool, info.Allocation.Mode, "allocation mode")
	require.Equal(t, "pool-runc", info.Allocation.PoolRef, "allocation pool ref")
	require.Equal(t, AllocationStateAllocated, info.Allocation.State, "allocation state")
}

func TestSandboxInfoAllocationAbsentIsOmitted(t *testing.T) {
	info := SandboxInfo{
		ID:        "sbx-unpooled",
		Status:    SandboxStatus{State: StateRunning},
		CreatedAt: mustParseTime(t, "2026-07-10T00:00:00Z"),
	}

	body, err := json.Marshal(info)
	require.NoErrorf(t, err, "marshal sandbox without allocation")
	var fields map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(body, &fields), "unmarshal sandbox JSON")
	_, present := fields["allocation"]
	if present {
		t.Fatal("absent allocation should remain omitted")
	}
}

func TestLifecycleClientAllocationInGetAndListResponses(t *testing.T) {
	_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandboxes/sbx-pooled":
			_, _ = w.Write([]byte(`{
				"id":"sbx-pooled",
				"status":{"state":"Running"},
				"createdAt":"2026-07-10T00:00:00Z",
				"allocation":{"mode":"pool","poolRef":"pool-runc","state":"allocated"}
			}`))
		case "/sandboxes":
			_, _ = w.Write([]byte(`{
				"items":[{
					"id":"sbx-pooled",
					"status":{"state":"Running"},
					"createdAt":"2026-07-10T00:00:00Z",
					"allocation":{"mode":"pool","poolRef":"pool-runc","state":"allocated"}
				}],
				"pagination":{"page":1,"pageSize":20,"totalItems":1,"totalPages":1,"hasNextPage":false}
			}`))
		default:
			http.NotFound(w, r)
		}
	})

	info, err := client.GetSandbox(context.Background(), "sbx-pooled")
	require.NoErrorf(t, err, "get sandbox")
	require.NotNil(t, info.Allocation, "get allocation")
	require.Equal(t, "pool-runc", info.Allocation.PoolRef, "get allocation pool ref")

	list, err := client.ListSandboxes(context.Background(), ListOptions{})
	require.NoErrorf(t, err, "list sandboxes")
	require.Len(t, list.Items, 1)
	require.NotNil(t, list.Items[0].Allocation, "list allocation")
	require.Equal(t, AllocationModePool, list.Items[0].Allocation.Mode, "list allocation mode")
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoErrorf(t, err, "parse time")
	return parsed
}
