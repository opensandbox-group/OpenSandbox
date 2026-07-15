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

// Package session provides functionality for managing Jupyter sessions
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client is the client for session management
type Client struct {
	// baseURL is the base URL of the Jupyter server
	baseURL string

	// httpClient is the client for sending HTTP requests, with authentication support
	httpClient *http.Client
}

// NewClient creates a new session management client
func NewClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// ListSessions retrieves the list of all active sessions
func (c *Client) ListSessions() ([]*Session, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/sessions", c.baseURL)

	// Send GET request
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON response
	var sessions []*Session
	if err := json.Unmarshal(body, &sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return sessions, nil
}

// GetSession retrieves information about a specific session
func (c *Client) GetSession(sessionId string) (*Session, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/sessions/%s", c.baseURL, sessionId)

	// Send GET request
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON response
	var session Session
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &session, nil
}

// CreateSession creates a new session
func (c *Client) CreateSession(name, ipynb, kernel string) (*Session, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/sessions", c.baseURL)

	// Build request body
	reqBody := &SessionCreateRequest{
		Path: ipynb,
		Name: name,
		Type: DefaultSessionType,
		Kernel: &KernelSpec{
			Name: kernel,
		},
	}

	// Serialize request body to JSON
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	// Create POST request
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON response
	var session Session
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &session, nil
}

// DeleteSession deletes the specified session
func (c *Client) DeleteSession(sessionId string) error {
	// Build request URL
	url := fmt.Sprintf("%s/api/sessions/%s", c.baseURL, sessionId)

	// Create DELETE request
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	return nil
}

// CreateSessionWithOptions usingoption to create a new session
func (c *Client) CreateSessionWithOptions(options *SessionOptions) (*Session, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/sessions", c.baseURL)

	// Build request body
	reqBody := &SessionCreateRequest{
		Path: options.Path,
		Name: options.Name,
	}

	// set session type
	if options.Type != "" {
		reqBody.Type = options.Type
	} else {
		reqBody.Type = DefaultSessionType
	}

	// set kernel information
	if options.KernelID != "" {
		// If kernel ID is provided, use existing kernel
		reqBody.Kernel = &KernelSpec{
			ID: options.KernelID,
		}
	} else if options.KernelName != "" {
		// If kernel name is provided, start new kernel
		reqBody.Kernel = &KernelSpec{
			Name: options.KernelName,
		}
	}

	// Serialize request body to JSON
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	// Create POST request
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON response
	var session Session
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &session, nil
}
