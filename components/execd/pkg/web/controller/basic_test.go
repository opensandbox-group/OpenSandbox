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

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestBasicControllerRespondSuccess(t *testing.T) {
	ctx, rec := newTestContext(http.MethodGet, "/", nil)
	ctrl := &basicController{ctx: ctx}

	payload := map[string]string{"status": "ok"}
	ctrl.RespondSuccess(payload)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "ok", resp["status"])
}

func TestBasicControllerRespondError(t *testing.T) {
	ctx, rec := newTestContext(http.MethodGet, "/", nil)
	ctrl := &basicController{ctx: ctx}

	ctrl.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "boom")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeInvalidRequest, resp.Code)
	require.Equal(t, "boom", resp.Message)
}

func setupBasicController(method string) (*basicController, *httptest.ResponseRecorder) {
	ctx, w := newTestContext(method, "/", nil)
	ctrl := &basicController{ctx: ctx}
	return ctrl, w
}

func TestRespondSuccessWritesPayload(t *testing.T) {
	ctrl, w := setupBasicController(http.MethodGet)

	payload := map[string]string{"status": "ok"}
	ctrl.RespondSuccess(payload)

	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "ok", got["status"])
}

func TestRespondErrorAddsCodeAndMessage(t *testing.T) {
	ctrl, w := setupBasicController(http.MethodGet)

	ctrl.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid payload")

	require.Equal(t, http.StatusBadRequest, w.Code)
	var got model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, model.ErrorCodeInvalidRequest, got.Code)
	require.Equal(t, "invalid payload", got.Message)
}

func TestQueryInt64(t *testing.T) {
	ctrl := &basicController{}

	tests := []struct {
		name     string
		query    string
		def      int64
		expected int64
	}{
		{name: "valid number", query: "42", def: 0, expected: 42},
		{name: "empty uses default", query: "", def: 5, expected: 5},
		{name: "invalid uses default", query: "not-a-number", def: -1, expected: -1},
		{name: "negative number", query: "-10", def: 0, expected: -10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ctrl.QueryInt64(tt.query, tt.def)
			require.Equalf(t, tt.expected, got, "QueryInt64(%q, %d)", tt.query, tt.def)
		})
	}
}
