// Copyright 2025 The OpenSandbox Authors
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
	"unicode/utf8"

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
		log.Error("sql: init db server: %v", err)
		return err
	}

	err = c.db.PingContext(ctx)
	if err != nil {
		request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "DBPingError", EValue: err.Error()})
		log.Error("sql: ping db server: %v", err)
		return err
	}

	if c.queryReturnsRows(request.Code) {
		return c.executeSelectSQLQuery(ctx, request)
	}
	return c.executeUpdateSQLQuery(ctx, request)
}

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
			switch x := v.(type) {
			case nil:
				row[i] = nil
			case []byte:
				// MySQL's text protocol returns non-numeric columns as raw bytes.
				// Keep non-UTF-8 payloads lossless: json.Marshal would otherwise
				// silently replace invalid bytes with U+FFFD.
				if utf8.Valid(x) {
					row[i] = string(x)
				} else {
					row[i] = fmt.Sprintf("%v", x)
				}
			default:
				row[i] = fmt.Sprintf("%v", x)
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

// queryReturnsRows reports whether the statement should use the row executor.
// Leading whitespace and --, #, and /* */ comments are skipped. SHOW, DESCRIBE,
// DESC, and EXPLAIN return rows the same way SELECT does. WITH is left on the
// update path because WITH ... DELETE is not a query.
func (c *Controller) queryReturnsRows(query string) bool {
	switch firstSQLToken(query) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN":
		return true
	default:
		return false
	}
}

// firstSQLToken returns the first SQL word, skipping whitespace and comments.
func firstSQLToken(query string) string {
	i := 0
	for i < len(query) {
		switch {
		case query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r':
			i++
		case strings.HasPrefix(query[i:], "--") || query[i] == '#':
			nl := strings.IndexByte(query[i:], '\n')
			if nl < 0 {
				return ""
			}
			i += nl + 1
		case strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += end + 4
		default:
			j := i
			for j < len(query) {
				switch query[j] {
				case ' ', '\t', '\n', '\r', '(':
					return strings.ToUpper(query[i:j])
				default:
					j++
				}
			}
			return strings.ToUpper(query[i:])
		}
	}
	return ""
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
