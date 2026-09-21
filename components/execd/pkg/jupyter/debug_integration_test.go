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

package jupyter

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"testing"
)

// TestDebugServerIntegration logs real server interactions for debugging.
func TestDebugServerIntegration(t *testing.T) {
	jupyterURL := getEnv("JUPYTER_URL", "")
	jupyterToken := getEnv("JUPYTER_TOKEN", "")
	if jupyterURL == "" || jupyterToken == "" {
		t.Skip("JUPYTER_URL and JUPYTER_TOKEN environment variables must be set to run this test")
	}

	t.Logf("Connecting to Jupyter server: %s", jupyterURL)

	httpClient := &http.Client{
		Transport: &debugTransport{t: t},
	}

	client := NewClient(jupyterURL,
		WithToken(jupyterToken),
		WithHTTPClient(httpClient))

	t.Run("Validate Authentication", func(t *testing.T) {
		t.Logf("Calling ValidateAuth...")
		status, err := client.ValidateAuth()
		if err != nil {
			t.Fatalf("Authentication validation failed: %v", err)
		}
		t.Logf("Authentication validation successful! Status: %s", status)
	})

	t.Run("Get API Information", func(t *testing.T) {
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/api", jupyterURL), nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Token %s", jupyterToken))

		t.Logf("Sending request to /api endpoint...")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Logf("API request returned non-200 status code: %d %s", resp.StatusCode, resp.Status)
		} else {
			t.Logf("API request successful, status code: %d %s", resp.StatusCode, resp.Status)

			respDump, err := httputil.DumpResponse(resp, true)
			if err != nil {
				t.Logf("Unable to dump response: %v", err)
			} else {
				t.Logf("Response details:\n%s", string(respDump))
			}
		}
	})

	t.Run("Test Different Header Combinations", func(t *testing.T) {
		headerSets := []map[string]string{
			{
				"Authorization": fmt.Sprintf("Token %s", jupyterToken),
			},
			{
				"Authorization": fmt.Sprintf("Token %s", jupyterToken),
				"X-XSRFToken":   jupyterToken[:16], // Use first 16 characters of token as XSRF token attempt
			},
			{
				"Authorization": fmt.Sprintf("token %s", jupyterToken), // lowercase token
			},
			{
				"Cookie": fmt.Sprintf("_xsrf=%s; jupyter_token=%s", jupyterToken[:16], jupyterToken),
			},
		}

		for i, headers := range headerSets {
			t.Logf("Testing header combination #%d:", i+1)
			for k, v := range headers {
				t.Logf("  %s: %s", k, v)
			}

			req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/kernelspecs", jupyterURL), nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			for k, v := range headers {
				req.Header.Set(k, v)
			}

			t.Logf("Sending request to /api/kernelspecs endpoint...")
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("Failed to send request: %v", err)
			}
			defer resp.Body.Close()

			t.Logf("Response status code: %d %s", resp.StatusCode, resp.Status)
			if resp.StatusCode == http.StatusOK {
				t.Logf("Successfully found valid header combination!")

				respDump, err := httputil.DumpResponse(resp, true)
				if err != nil {
					t.Logf("Unable to dump response: %v", err)
				} else {
					maxLen := 500
					respStr := string(respDump)
					if len(respStr) > maxLen {
						t.Logf("Response (truncated):\n%s...", respStr[:maxLen])
					} else {
						t.Logf("Response:\n%s", respStr)
					}
				}
			}
		}
	})
}

// debugTransport logs request and response dumps.
type debugTransport struct {
	t *testing.T
}

func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqDump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		d.t.Logf("Unable to dump request: %v", err)
	} else {
		maxLen := 500
		reqStr := string(reqDump)
		if len(reqStr) > maxLen {
			d.t.Logf("Request (truncated):\n%s...", reqStr[:maxLen])
		} else {
			d.t.Logf("Request:\n%s", reqStr)
		}
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	d.t.Logf("Response status: %d %s", resp.StatusCode, resp.Status)

	return resp, nil
}
