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

package opensandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLifecycleMutationInvalidatesEndpointCache(t *testing.T) {
	for _, operation := range []string{"pause", "resume", "delete"} {
		for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				reads := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/endpoints/") {
						reads++
						fmt.Fprintf(w, `{"endpoint":"host-%d:8080"}`, reads)
						return
					}
					w.WriteHeader(status)
				}))
				defer server.Close()
				client := NewLifecycleClient(server.URL, "")
				ctx := context.Background()
				before, err := client.GetEndpoint(ctx, "sandbox", 8080, nil)
				if err != nil {
					t.Fatal(err)
				}
				other := endpointCacheKey{sandboxID: "other", port: 8080}
				client.cache.Put(other, &Endpoint{Endpoint: "unchanged"})
				variants := []endpointCacheKey{{sandboxID: "sandbox", port: 9090}, {sandboxID: "sandbox", port: 8080, useServerProxy: true}}
				for _, key := range variants {
					client.cache.Put(key, &Endpoint{Endpoint: "stale"})
				}
				switch operation {
				case "pause":
					err = client.PauseSandbox(ctx, "sandbox")
				case "resume":
					err = client.ResumeSandbox(ctx, "sandbox")
				case "delete":
					err = client.DeleteSandbox(ctx, "sandbox")
				}
				if (err != nil) != (status >= 400) {
					t.Fatalf("mutation error = %v", err)
				}
				after, err := client.GetEndpoint(ctx, "sandbox", 8080, nil)
				if err != nil {
					t.Fatal(err)
				}
				if status < 400 && before.Endpoint == after.Endpoint {
					t.Errorf("successful mutation retained stale endpoint %q", after.Endpoint)
				}
				if status >= 400 && (before.Endpoint != after.Endpoint || reads != 1) {
					t.Errorf("failed mutation evicted endpoint: reads=%d", reads)
				}
				for _, key := range variants {
					if _, ok := client.cache.Get(key); ok != (status >= 400) {
						t.Errorf("cache presence for %+v = %v", key, ok)
					}
				}
				if ep, ok := client.cache.Get(other); !ok || ep.Endpoint != "unchanged" {
					t.Error("unrelated sandbox cache was invalidated")
				}
			})
		}
	}
}

func TestLifecycleMutationWithoutCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	client := NewLifecycleClientWithCache(server.URL, "", nil)
	for _, mutate := range []func(context.Context, string) error{client.PauseSandbox, client.ResumeSandbox, client.DeleteSandbox} {
		if err := mutate(context.Background(), "sandbox"); err != nil {
			t.Fatal(err)
		}
	}
}
