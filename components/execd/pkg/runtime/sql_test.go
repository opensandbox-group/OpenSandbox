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

package runtime

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

func TestExecuteSelectSQLQuery_Success(t *testing.T) {
	driver := &stubDriver{
		columns: []string{"id", "name"},
		rows: [][]driver.Value{
			{int64(1), "alice"},
			{int64(2), "bob"},
		},
	}
	db := newStubDB(t, driver)

	c := NewController("", "")
	c.db = db

	var (
		gotResult map[string]any
		gotError  *execute.ErrorOutput
		completed bool
	)

	req := &ExecuteCodeRequest{
		Code: "SELECT * FROM users",
		Hooks: ExecuteResultHook{
			OnExecuteResult: func(result map[string]any, _ int) {
				gotResult = result
			},
			OnExecuteError: func(err *execute.ErrorOutput) {
				gotError = err
			},
			OnExecuteComplete: func(time.Duration) {
				completed = true
			},
		},
	}

	require.NoError(t, c.executeSelectSQLQuery(context.Background(), req))

	require.Nil(t, gotError, "unexpected error hook")
	require.True(t, completed, "expected completion hook to be triggered")

	raw, ok := gotResult["text/plain"]
	require.True(t, ok, "expected text/plain payload")
	var qr QueryResult
	require.NoError(t, json.Unmarshal([]byte(raw.(string)), &qr))

	require.Equal(t, []string{"id", "name"}, qr.Columns, "unexpected columns")
	require.Len(t, qr.Rows, 2, "unexpected rows")
	require.Equal(t, "1", qr.Rows[0][0])
	require.Equal(t, "bob", qr.Rows[1][1])
}

func TestExecuteUpdateSQLQuery_Success(t *testing.T) {
	driver := &stubDriver{
		execRowsAffected: 3,
	}
	db := newStubDB(t, driver)

	c := NewController("", "")
	c.db = db

	var (
		gotResult map[string]any
		gotError  *execute.ErrorOutput
		completed bool
	)

	req := &ExecuteCodeRequest{
		Code: "UPDATE users SET name='alice' WHERE id=1",
		Hooks: ExecuteResultHook{
			OnExecuteResult: func(result map[string]any, _ int) {
				gotResult = result
			},
			OnExecuteError: func(err *execute.ErrorOutput) {
				gotError = err
			},
			OnExecuteComplete: func(time.Duration) {
				completed = true
			},
		},
	}

	require.NoError(t, c.executeUpdateSQLQuery(context.Background(), req))

	require.Nil(t, gotError, "unexpected error hook")
	require.True(t, completed, "expected completion hook to be triggered")

	raw, ok := gotResult["text/plain"]
	require.True(t, ok, "expected text/plain payload")
	var qr QueryResult
	require.NoError(t, json.Unmarshal([]byte(raw.(string)), &qr))

	require.Equal(t, []string{"affected_rows"}, qr.Columns, "unexpected columns")
	require.Len(t, qr.Rows, 1, "unexpected rows length")
	require.Len(t, qr.Rows[0], 1, "unexpected row entry length")
	require.Equal(t, float64(3), qr.Rows[0][0])
}
