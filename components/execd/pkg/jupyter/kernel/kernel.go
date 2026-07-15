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

// Package kernel provides functionality for managing Jupyter kernels
package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client is the client for kernel management
type Client struct {
	// baseURL is the base URL of the Jupyter server
	baseURL string

	// httpClient is the client for sending HTTP requests, with authentication support
	httpClient *http.Client
}

// NewClient creates a new kernel management client
func NewClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// GetKernelSpecs retrieves the list of available kernel specifications
func (c *Client) GetKernelSpecs() (*KernelSpecs, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernelspecs", c.baseURL)

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
	var specs KernelSpecs
	if err := json.Unmarshal(body, &specs); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &specs, nil
}

// ListKernels retrieves the list of all running kernels
func (c *Client) ListKernels() ([]*Kernel, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernels", c.baseURL)

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
	var kernels []*Kernel
	if err := json.Unmarshal(body, &kernels); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return kernels, nil
}

// GetKernel retrieves information about a specific kernel
func (c *Client) GetKernel(kernelId string) (*Kernel, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernels/%s", c.baseURL, kernelId)

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
	var kernel Kernel
	if err := json.Unmarshal(body, &kernel); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &kernel, nil
}

// StartKernel starts a new kernel
func (c *Client) StartKernel(name string) (*Kernel, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernels", c.baseURL)

	// Build request body
	reqBody := &KernelStartRequest{
		Name: name,
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
	var kernel Kernel
	if err := json.Unmarshal(body, &kernel); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &kernel, nil
}

// RestartKernel restarts the specified kernel
func (c *Client) RestartKernel(kernelId string) (bool, error) {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernels/%s/restart", c.baseURL, kernelId)

	// Create POST request
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("server returned error status code: %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON response
	var response KernelRestartResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return false, fmt.Errorf("failed to parse response: %w", err)
	}

	return response.Restarted, nil
}

// InterruptKernel interrupts the specified kernel
func (c *Client) InterruptKernel(kernelId string) error {
	// Build request URL
	url := fmt.Sprintf("%s/api/kernels/%s/interrupt", c.baseURL, kernelId)

	// Create POST request
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
