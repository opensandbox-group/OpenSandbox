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
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type countedBody struct {
	io.Reader
	read int
}

func (c *countedBody) Read(b []byte) (int, error) { n, e := c.Reader.Read(b); c.read += n; return n, e }
func TestCreationBodyReadLimit(t *testing.T) {
	const limit = 1 << 20
	valid := `{"operation_id":"operation","command":"true"}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"exact-limit", valid + strings.Repeat(" ", limit-len(valid)), true},
		{"first-json-oversized", `{"operation_id":"operation","command":"` + strings.Repeat("a", 8<<20) + `"}`, false},
		{"trailing-json-oversized", valid + ` "` + strings.Repeat("a", 8<<20) + `"`, false},
		{"trailing-whitespace-oversized", valid + strings.Repeat(" ", 8<<20), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			reader := &countedBody{Reader: strings.NewReader(tt.body)}
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/command/operations", reader)
			var request model.RunCommandRequest
			err := newBasicController(ctx).decodeCreation(&request, true)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.LessOrEqual(t, reader.read, limit+1)
		})
	}
}

func TestOperationCommandDecoderRejectsAmbiguousInputs(t *testing.T) {
	for _, fields := range []string{
		`"command":"true","unknown":"secret"`,
		`"argv":["true"],"unknown":"secret"`,
		`"command":"true","argv":["false"]`,
		`"command":null`, `"argv":null`, `"argv":["true",null]`,
	} {
		t.Run(fields, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/command/operations", strings.NewReader(`{"operation_id":"operation",`+fields+`}`))
			var request model.RunCommandRequest
			err := newBasicController(ctx).decodeCreation(&request, true)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/command", strings.NewReader(`{"operation_id":"ignored","command":"true","unknown":"legacy"}`))
	var legacy model.RunCommandRequest
	require.NoError(t, newBasicController(ctx).decodeCreation(&legacy, false), "legacy decoding must keep accepting unknown fields")
}
