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
	"net/url"
	"time"
)

// DiagnosticContext identifies the backend and its diagnostic identifiers.
type DiagnosticContext struct {
	SandboxID          string            `json:"sandboxId"`
	State              *string           `json:"state,omitempty"`
	BackendType        string            `json:"backendType"`
	BackendIdentifiers map[string]string `json:"backendIdentifiers"`
}

// DiagnosticContent describes diagnostic text. URL content is not downloaded automatically.
type DiagnosticContent struct {
	SandboxID     string     `json:"sandboxId"`
	Kind          string     `json:"kind"`
	Scope         string     `json:"scope"`
	Delivery      string     `json:"delivery"`
	ContentType   string     `json:"contentType"`
	Truncated     bool       `json:"truncated"`
	Content       *string    `json:"content,omitempty"`
	ContentURL    *string    `json:"contentUrl,omitempty"`
	ContentLength *int64     `json:"contentLength,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
}

// GetDiagnosticContext retrieves backend context without connecting to execd.
func (c *LifecycleClient) GetDiagnosticContext(ctx context.Context, sandboxID string) (*DiagnosticContext, error) {
	var result DiagnosticContext
	err := c.doRequest(ctx, "GET", "/sandboxes/"+url.PathEscape(sandboxID)+"/diagnostics/context", nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *LifecycleClient) getDiagnosticContent(ctx context.Context, sandboxID, scope, kind string) (*DiagnosticContent, error) {
	var result DiagnosticContent
	path := "/sandboxes/" + url.PathEscape(sandboxID) + "/diagnostics/" + kind + "?" + url.Values{"scope": {scope}}.Encode()
	if err := c.doRequest(ctx, "GET", path, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetDiagnosticLogs retrieves a scoped diagnostic log descriptor.
func (c *LifecycleClient) GetDiagnosticLogs(ctx context.Context, sandboxID, scope string) (*DiagnosticContent, error) {
	return c.getDiagnosticContent(ctx, sandboxID, scope, "logs")
}

// GetDiagnosticEvents retrieves a scoped diagnostic event descriptor.
func (c *LifecycleClient) GetDiagnosticEvents(ctx context.Context, sandboxID, scope string) (*DiagnosticContent, error) {
	return c.getDiagnosticContent(ctx, sandboxID, scope, "events")
}

// GetDiagnosticContext retrieves backend context without connecting to execd.
func (m *SandboxManager) GetDiagnosticContext(ctx context.Context, sandboxID string) (*DiagnosticContext, error) {
	return m.lifecycle.GetDiagnosticContext(ctx, sandboxID)
}

// GetDiagnosticLogs retrieves a scoped diagnostic log descriptor.
func (m *SandboxManager) GetDiagnosticLogs(ctx context.Context, sandboxID, scope string) (*DiagnosticContent, error) {
	return m.lifecycle.GetDiagnosticLogs(ctx, sandboxID, scope)
}

// GetDiagnosticEvents retrieves a scoped diagnostic event descriptor.
func (m *SandboxManager) GetDiagnosticEvents(ctx context.Context, sandboxID, scope string) (*DiagnosticContent, error) {
	return m.lifecycle.GetDiagnosticEvents(ctx, sandboxID, scope)
}

// GetDiagnosticContext retrieves this sandbox's backend context.
func (s *Sandbox) GetDiagnosticContext(ctx context.Context) (*DiagnosticContext, error) {
	return s.lifecycle.GetDiagnosticContext(ctx, s.id)
}

// GetDiagnosticLogs retrieves this sandbox's scoped diagnostic log descriptor.
func (s *Sandbox) GetDiagnosticLogs(ctx context.Context, scope string) (*DiagnosticContent, error) {
	return s.lifecycle.GetDiagnosticLogs(ctx, s.id, scope)
}

// GetDiagnosticEvents retrieves this sandbox's scoped diagnostic event descriptor.
func (s *Sandbox) GetDiagnosticEvents(ctx context.Context, scope string) (*DiagnosticContent, error) {
	return s.lifecycle.GetDiagnosticEvents(ctx, s.id, scope)
}
