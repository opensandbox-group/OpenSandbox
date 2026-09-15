// Copyright 2025 Alibaba Group Holding Ltd.
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

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestNewRouterCommandInventory(t *testing.T) {
	controller.InitCodeRunner()
	router := NewRouter("")
	req := httptest.NewRequest(http.MethodGet, "/command", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
}

func TestNewRouterCommandRoutesRemainRegistered(t *testing.T) {
	router := NewRouter("")

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "post command", method: http.MethodPost, path: "/command"},
		{name: "delete command", method: http.MethodDelete, path: "/command"},
		{name: "get command status", method: http.MethodGet, path: "/command/status/:id"},
		{name: "get command logs", method: http.MethodGet, path: "/command/:id/logs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			found := false
			for _, route := range router.Routes() {
				if route.Method == test.method && route.Path == test.path {
					found = true
					break
				}
			}
			require.True(t, found, "%s %s must remain registered", test.method, test.path)
		})
	}
}

func TestNewRouterAccessToken(t *testing.T) {
	controller.InitCodeRunner()
	router := NewRouter("expected-token")

	t.Run("command inventory requires configured token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/command", nil)
		response := httptest.NewRecorder()

		router.ServeHTTP(response, req)

		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	})

	t.Run("command inventory permits configured token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/command", nil)
		req.Header.Set(model.ApiAccessTokenHeader, "expected-token")
		response := httptest.NewRecorder()

		router.ServeHTTP(response, req)

		require.Equal(t, http.StatusOK, response.Code)
	})
}
