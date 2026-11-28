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
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLifecycleListPagination(t *testing.T) {
	ctx := context.Background()
	endpoints := []struct {
		path string
		list func(*LifecycleClient, int, int) error
	}{
		{"/sandboxes", func(client *LifecycleClient, page, pageSize int) error {
			_, err := client.ListSandboxes(ctx, ListOptions{Page: page, PageSize: pageSize})
			return err
		}},
		{"/snapshots", func(client *LifecycleClient, page, pageSize int) error {
			_, err := client.ListSnapshots(ctx, ListSnapshotsOptions{Page: page, PageSize: pageSize})
			return err
		}},
		{"/templates", func(client *LifecycleClient, page, pageSize int) error {
			_, err := client.ListTemplates(ctx, ListTemplatesOptions{Page: page, PageSize: pageSize})
			return err
		}},
	}
	cases := []struct {
		name     string
		page     int
		pageSize int
		query    string
		errField string
	}{
		{name: "defaults"},
		{name: "explicit_pagination", page: 2, pageSize: 200, query: "page=2&pageSize=200"},
		{name: "default_page", pageSize: 1, query: "pageSize=1"},
		{name: "default_page_size", page: 1, query: "page=1"},
		{name: "negative_page", page: -1, pageSize: 20, errField: "page"},
		{name: "negative_page_size", page: 2, pageSize: -1, errField: "pageSize"},
	}
	for _, endpoint := range endpoints {
		t.Run(strings.TrimPrefix(endpoint.path, "/"), func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					requests := make(chan url.URL, 1)
					_, client := newLifecycleServer(t, func(w http.ResponseWriter, r *http.Request) {
						requests <- *r.URL
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"items":[],"pagination":{}}`)
					})
					err := endpoint.list(client, tc.page, tc.pageSize)
					if tc.errField != "" {
						if err == nil || !strings.Contains(err.Error(), tc.errField+" must ") {
							t.Errorf("error = %v, want validation error for %s", err, tc.errField)
						}
						select {
						case request := <-requests:
							t.Errorf("unexpected request for invalid pagination: %s", request.String())
						default:
						}
						return
					}
					if err != nil {
						t.Fatalf("list: %v", err)
					}
					select {
					case request := <-requests:
						if request.Path != endpoint.path || request.RawQuery != tc.query {
							t.Errorf("request = %s, want path %s and query %q", request.String(), endpoint.path, tc.query)
						}
					default:
						t.Fatal("list returned without sending a request")
					}
				})
			}
		})
	}
}
