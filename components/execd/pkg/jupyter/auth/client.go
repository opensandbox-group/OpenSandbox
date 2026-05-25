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

package auth

import (
	"fmt"
	"io"
	"net/http"
)

// Client wraps http.Client and injects auth headers.
type Client struct {
	httpClient *http.Client
	auth       *Auth
}

// NewClient creates a new authenticated HTTP client.
func NewClient(httpClient *http.Client, auth *Auth) *Client {
	return &Client{
		httpClient: httpClient,
		auth:       auth,
	}
}

// Do sends an HTTP request and automatically adds authentication data.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c.auth == nil {
		return c.httpClient.Do(req)
	}

	if c.auth.Token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("token %s", c.auth.Token))
	} else if c.auth.Username != "" {
		req.SetBasicAuth(c.auth.Username, c.auth.Password)
	}

	return c.httpClient.Do(req)
}

// Get sends a GET request.
func (c *Client) Get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post sends a POST request.
func (c *Client) Post(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// Put sends a PUT request.
func (c *Client) Put(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// Delete sends a DELETE request.
func (c *Client) Delete(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
