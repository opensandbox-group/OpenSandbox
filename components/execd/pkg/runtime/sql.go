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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "github.com/go-sql-driver/mysql"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// QueryResult represents a SQL query response.
type QueryResult struct {
	Columns []string `json:"columns,omitempty"`
	Rows    [][]any  `json:"rows,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// runSQL executes SQL queries based on their type.
func (c *Controller) runSQL(ctx context.Context, request *ExecuteCodeRequest) error {
	request.Hooks.OnExecuteInit(uuid.New().String())
	err := c.initDB()
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBInitError", EValue: err.Error()})
		log.Error("DBInitError: error initializing db server: %v", err)
		return err
	}

	err = c.db.PingContext(ctx)
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBPingError", EValue: err.Error()})
		log.Error("DBPingError: error pinging db server: %v", err)
		return err
	}

	switch c.getQueryType(request.Code) {
	case "SELECT":
		return c.executeSelectSQLQuery(ctx, request)
	default:
		return c.executeUpdateSQLQuery(ctx, request)
	}
}

// executeSelectSQLQuery handles SELECT statements.
func (c *Controller) executeSelectSQLQuery(ctx context.Context, request *ExecuteCodeRequest) error {
	startAt := time.Now()

	// The SQL runtime intentionally executes the user's submitted SQL program
	// against the sandbox-local database; no trusted query template is composed here.
	rows, err := c.db.QueryContext(ctx, request.Code) // lgtm[go/sql-injection]
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBQueryError", EValue: err.Error()})
		return nil
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBQueryError", EValue: err.Error()})
		return nil
	}

	var result [][]any
	values := make([]any, len(columns))
	scanArgs := make([]any, len(columns))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	for rows.Next() {
		err := rows.Scan(scanArgs...)
		if err != nil {
			request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "RowScanError", EValue: err.Error()})
			return nil
		}
		row := make([]any, len(columns))
		for i, v := range values {
			if v == nil {
				row[i] = nil
			} else {
				row[i] = fmt.Sprintf("%v", v)
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "RowIterationError", EValue: err.Error()})
		return nil
	}

	queryResult := QueryResult{
		Columns: columns,
		Rows:    result,
	}
	bytes, err := json.Marshal(queryResult)
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "JSONMarshalError", EValue: err.Error()})
		return nil
	}
	request.Hooks.OnExecuteResult(
		map[string]any{
			"text/plain": string(bytes),
		},
		1,
	)
	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}

// executeUpdateSQLQuery handles non-SELECT statements.
func (c *Controller) executeUpdateSQLQuery(ctx context.Context, request *ExecuteCodeRequest) error {
	startAt := time.Now()

	// The SQL runtime intentionally executes the user's submitted SQL program
	// against the sandbox-local database; no trusted query template is composed here.
	result, err := c.db.ExecContext(ctx, request.Code) // lgtm[go/sql-injection]
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBExecError", EValue: err.Error()})
		return err
	}

	affected, _ := result.RowsAffected()
	queryResult := QueryResult{
		Rows:    [][]any{{affected}},
		Columns: []string{"affected_rows"},
	}
	bytes, err := json.Marshal(queryResult)
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "JSONMarshalError", EValue: err.Error()})
		return err
	}
	request.Hooks.OnExecuteResult(
		map[string]any{
			"text/plain": string(bytes),
		},
		1,
	)
	request.Hooks.OnExecuteComplete(time.Since(startAt))
	return nil
}

// getQueryType extracts the first token to decide which executor to use.
func (c *Controller) getQueryType(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// initDB lazily opens the local sandbox database.
func (c *Controller) initDB() error {
	var initErr error
	c.dbOnce.Do(func() {
		dsn := "root:@tcp(127.0.0.1:3306)/"
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			initErr = err
			return
		}

		err = db.Ping()
		if err != nil {
			initErr = err
			return
		}

		_, err = db.Exec("CREATE DATABASE IF NOT EXISTS sandbox")
		if err != nil {
			initErr = err
			return
		}

		_, err = db.Exec("USE sandbox")
		if err != nil {
			initErr = err
			return
		}

		c.db = db
	})

	if initErr != nil {
		return initErr
	}
	if c.db == nil {
		return errors.New("db is not initialized")
	}
	return nil
}
