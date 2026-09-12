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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ExecutionInstance identifies one execd lifetime and its bounded recovery window.
type ExecutionInstance struct {
	InstanceID       string `json:"instance_id"`
	IssuedAt         int64  `json:"issued_at"`
	RetentionSeconds int64  `json:"retention_seconds"`
	Capacity         int    `json:"capacity"`
}

// NewOperationID creates a caller identity. Persist it BEFORE calling a create
// method. Never regenerate it on recovery, expiry or an instance mismatch.
func (i ExecutionInstance) NewOperationID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%d.%s", i.InstanceID, i.IssuedAt, hex.EncodeToString(token[:])), nil
}

// ExecutionOperation reports creation only, never command completion or success.
type ExecutionOperation struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
}

type executionInstanceFetch struct {
	done  chan struct{}
	value ExecutionInstance
	err   error
}

type executionInstanceCache struct {
	sync.Mutex
	value      ExecutionInstance
	expires    time.Time
	pending    *executionInstanceFetch
	generation uint64
}

// GetExecutionInstance reuses a private server snapshot for at most one minute.
// Call for each new operation; recovery must load the previously persisted ID.
func (e *ExecdClient) GetExecutionInstance(ctx context.Context) (*ExecutionInstance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache := &e.instanceCache
	cache.Lock()
	if time.Now().Before(cache.expires) {
		result := cache.value
		cache.Unlock()
		return &result, nil
	}
	if pending := cache.pending; pending != nil {
		cache.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			result := pending.value
			return &result, pending.err
		}
	}
	pending := &executionInstanceFetch{done: make(chan struct{})}
	cache.pending = pending
	generation := cache.generation
	started := time.Now()
	cache.Unlock()

	var result ExecutionInstance
	err := e.client.doRequest(ctx, http.MethodGet, "/execution/instance", nil, &result)
	cache.Lock()
	pending.value, pending.err = result, err
	if generation == cache.generation && err == nil {
		cache.value = result
		cache.expires = started.Add(time.Minute)
	}
	if cache.pending == pending {
		cache.pending = nil
	}
	close(pending.done)
	cache.Unlock()
	return &result, err
}

func (e *ExecdClient) invalidateOperationInstance(err error) {
	var apiError *APIError
	if errors.As(err, &apiError) && (apiError.Response.Code == "operation_instance_mismatch" || apiError.Response.Code == "operation_expired") {
		cache := &e.instanceCache
		cache.Lock()
		cache.expires = time.Time{}
		cache.pending = nil
		cache.generation++
		cache.Unlock()
	}
}

func (e *ExecdClient) GetExecutionOperation(ctx context.Context, kind, operationID string) (*ExecutionOperation, error) {
	// Per-call copy prevents concurrent lookups from sharing a mutable identity header.
	client := *e.client
	client.headers = maps.Clone(e.client.headers)
	if client.headers == nil {
		client.headers = make(map[string]string)
	}
	client.headers["X-EXECD-OPERATION-ID"] = operationID
	var result ExecutionOperation
	err := client.doRequest(ctx, http.MethodGet, "/execution/operation?kind="+url.QueryEscape(kind), nil, &result)
	e.invalidateOperationInstance(err)
	return &result, err
}

// CreateCommandOperation opts into JSON creation acknowledgement (no SSE). Reuse
// exactly the same ID and request after response loss; poll status using result.ID.
func (e *ExecdClient) CreateCommandOperation(ctx context.Context, operationID string, request RunCommandRequest) (*ExecutionOperation, error) {
	if operationID == "" {
		return nil, fmt.Errorf("opensandbox: operation identity is required")
	}
	body := struct {
		RunCommandRequest
		OperationID string `json:"operation_id"`
	}{request, operationID}
	var result ExecutionOperation
	err := e.client.doRequest(ctx, http.MethodPost, "/command/operations", body, &result)
	e.invalidateOperationInstance(err)
	return &result, err
}

// CreatePTYOperation creates/reconciles a dormant session. Connect its recovered
// ID with the existing PTY WebSocket protocol; this call does not start a shell.
func (e *ExecdClient) CreatePTYOperation(ctx context.Context, operationID, cwd, command string) (*ExecutionOperation, error) {
	if operationID == "" {
		return nil, fmt.Errorf("opensandbox: operation identity is required")
	}
	body := struct {
		OperationID string `json:"operation_id"`
		Cwd         string `json:"cwd,omitempty"`
		Command     string `json:"command,omitempty"`
	}{operationID, cwd, command}
	var result ExecutionOperation
	err := e.client.doRequest(ctx, http.MethodPost, "/pty/operations", body, &result)
	e.invalidateOperationInstance(err)
	return &result, err
}
