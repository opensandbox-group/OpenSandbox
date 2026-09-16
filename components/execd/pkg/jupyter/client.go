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

package jupyter

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/auth"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/kernel"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/session"
)

// Client interacts with the Jupyter server.
type Client struct {
	BaseURL       string
	httpClient    *http.Client
	Auth          *auth.Auth
	kernelClient  *kernel.Client
	sessionClient *session.Client
	executeClient *execute.Client
	authClient    *auth.Client
}

type ClientOption func(*Client)

func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

func WithToken(token string) ClientOption {
	return func(c *Client) {
		c.Auth.Token = token
	}
}

func NewClient(baseURL string, options ...ClientOption) *Client {
	client := &Client{
		BaseURL:    baseURL,
		httpClient: http.DefaultClient,
		Auth:       auth.NewAuth(),
	}

	for _, option := range options {
		option(client)
	}

	client.authClient = auth.NewClient(client.httpClient, client.Auth)

	client.kernelClient = kernel.NewClient(baseURL, client.httpClient)
	client.sessionClient = session.NewClient(baseURL, client.httpClient)
	client.executeClient = execute.NewClient(baseURL, client.authClient)

	return client
}

func (c *Client) SetToken(token string) {
	c.Auth.Token = token
}

// ValidateAuth quickly checks that some auth data is present.
func (c *Client) ValidateAuth() (string, error) {
	authType := c.Auth.Validate()
	if authType == "none" {
		return "error", errors.New("no valid authentication information provided")
	}
	return "ok", nil
}

func (c *Client) GetKernelSpecs() (*kernel.KernelSpecs, error) {
	return c.kernelClient.GetKernelSpecs()
}

func (c *Client) ListKernels() ([]*kernel.Kernel, error) {
	return c.kernelClient.ListKernels()
}

func (c *Client) GetKernel(kernelId string) (*kernel.Kernel, error) {
	return c.kernelClient.GetKernel(kernelId)
}

func (c *Client) StartKernel(name string) (*kernel.Kernel, error) {
	return c.kernelClient.StartKernel(name)
}

func (c *Client) RestartKernel(kernelId string) (bool, error) {
	return c.kernelClient.RestartKernel(kernelId)
}

func (c *Client) InterruptKernel(kernelId string) error {
	return c.kernelClient.InterruptKernel(kernelId)
}

func (c *Client) ListSessions() ([]*session.Session, error) {
	return c.sessionClient.ListSessions()
}

func (c *Client) GetSession(sessionId string) (*session.Session, error) {
	return c.sessionClient.GetSession(sessionId)
}

func (c *Client) CreateSession(name, ipynb, kernel string) (*session.Session, error) {
	return c.sessionClient.CreateSession(name, ipynb, kernel)
}

func (c *Client) DeleteSession(sessionId string) error {
	return c.sessionClient.DeleteSession(sessionId)
}

func (c *Client) ConnectToKernel(kernelId string) error {
	parsedURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}

	scheme := "ws"
	if parsedURL.Scheme == "https" {
		scheme = "wss"
	}

	wsURL := fmt.Sprintf("%s://%s/api/kernels/%s/channels", scheme, parsedURL.Host, kernelId)

	if c.Auth.Token != "" {
		wsURL = fmt.Sprintf("%s?token=%s", wsURL, c.Auth.Token)
	}

	return c.executeClient.Connect(wsURL)
}

func (c *Client) DisconnectFromKernel() {
	c.executeClient.Disconnect()
}

func (c *Client) ExecuteCodeStream(kernelId, code string, resultChan chan *execute.ExecutionResult) error {
	return c.executeClient.ExecuteCodeStream(code, resultChan)
}

func (c *Client) ExecuteCodeWithCallback(code string, handler execute.CallbackHandler) error {
	return c.executeClient.ExecuteCodeWithCallback(code, handler)
}
