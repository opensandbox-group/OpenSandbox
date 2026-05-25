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
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubDriver struct {
	columns          []string
	rows             [][]driver.Value
	execRowsAffected int64
	queryErr         error
	execErr          error
	pingErr          error
	execCalled       int32
	queryCalled      int32
}

type stubConn struct {
	d *stubDriver
}

func (c *stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *stubConn) Close() error                        { return nil }
func (c *stubConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c *stubConn) Ping(context.Context) error {
	return c.d.pingErr
}

func (c *stubConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	atomic.AddInt32(&c.d.execCalled, 1)
	if c.d.execErr != nil {
		return nil, c.d.execErr
	}
	return driver.RowsAffected(c.d.execRowsAffected), nil
}

func (c *stubConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	atomic.AddInt32(&c.d.queryCalled, 1)
	if c.d.queryErr != nil {
		return nil, c.d.queryErr
	}
	return &stubRows{
		columns: c.d.columns,
		rows:    c.d.rows,
	}, nil
}

type stubRows struct {
	columns []string
	rows    [][]driver.Value
	idx     int
}

func (r *stubRows) Columns() []string { return r.columns }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	for i, v := range row {
		dest[i] = v
	}
	return nil
}

type stubConnector struct {
	d *stubDriver
}

func (c *stubConnector) Connect(context.Context) (driver.Conn, error) {
	return &stubConn{d: c.d}, nil
}

func (c *stubConnector) Driver() driver.Driver {
	return c
}

func (c *stubConnector) Open(string) (driver.Conn, error) {
	return &stubConn{d: c.d}, nil
}

func newStubDB(t *testing.T, d *stubDriver) *sql.DB {
	t.Helper()
	driverName := fmt.Sprintf("stub-%d", time.Now().UnixNano())
	sql.Register(driverName, &stubConnector{d: d})
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	return db
}
