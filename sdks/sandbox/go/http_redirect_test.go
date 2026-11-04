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
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedirectProtectedHeadersOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, initial, target string
		same                  bool
	}{
		{"same origin", "http://example.com/a", "http://example.com/b", true},
		{"default HTTP port", "http://EXAMPLE.com/a", "http://example.com:80/b", true},
		{"default HTTPS port", "https://example.com:443/a", "https://example.com/b", true},
		{"scheme", "http://example.com/a", "https://example.com/b", false},
		{"host", "http://example.com/a", "http://other.example.com/b", false},
		{"port", "http://example.com/a", "http://example.com:8080/b", false},
		{"IPv6", "http://[::1]/a", "http://[::1]:80/b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial, err := http.NewRequest(http.MethodGet, tc.initial, nil)
			require.NoError(t, err)
			target, err := http.NewRequest(http.MethodGet, tc.target, nil)
			require.NoError(t, err)
			target.Header = http.Header{
				"oPeN-SaNdBoX-Api-Key":      {"key"},
				"opensandbox-secure-access": {"secret"},
				"OPENSANDBOX-EGRESS-AUTH":   {"egress"},
				"X-Custom":                  {"keep"},
			}
			require.NoError(t, protectedHeaderRedirectPolicy(nil, "OPEN-SANDBOX-API-KEY")(target, []*http.Request{initial}))
			require.Equal(t, "keep", target.Header.Get("X-Custom"))
			if tc.same {
				require.Equal(t, 4, len(target.Header))
			} else {
				require.Equal(t, 1, len(target.Header))
			}
		})
	}
}

func TestRedirectCustomPolicyAndClientIsolation(t *testing.T) {
	sentinel := errors.New("redirect rejected")
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	transport := DefaultTransport()
	for _, policyErr := range []error{nil, sentinel, http.ErrUseLastResponse} {
		t.Run(fmtPolicyName(policyErr), func(t *testing.T) {
			calls := 0
			original := &http.Client{
				Timeout: time.Minute, Transport: transport, Jar: jar,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					calls++
					req.Header["oPeNsAnDbOx-Secure-Access"] = []string{"re-added"}
					req.Header["x-CuStOm-AuTh"] = []string{"re-added-token"}
					return policyErr
				},
			}
			client := NewClient("http://example.com", "key", "OPEN-SANDBOX-API-KEY",
				WithHTTPClient(original), WithTimeout(time.Second), WithAuthHeader("X-Custom-Auth"))
			require.True(t, client.httpClient != original)
			require.Equal(t, time.Minute, original.Timeout)
			require.True(t, client.httpClient.Transport == transport)
			require.True(t, client.httpClient.Jar == jar)
			initial, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			require.NoError(t, err)
			target, err := http.NewRequest(http.MethodGet, "http://other.example.com", nil)
			require.NoError(t, err)
			got := client.httpClient.CheckRedirect(target, []*http.Request{initial})
			require.True(t, got == policyErr)
			require.Equal(t, 1, calls)
			if policyErr == nil {
				require.Equal(t, 0, len(target.Header))
			}
			// The caller's policy remains untouched.
			target.Header = make(http.Header)
			_ = original.CheckRedirect(target, []*http.Request{initial})
			require.Equal(t, 2, len(target.Header))
		})
	}
}

func fmtPolicyName(err error) string {
	if err == nil {
		return "allow"
	}
	return err.Error()
}

func TestRedirectDefaultLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, "", "OPEN-SANDBOX-API-KEY")
	req, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	require.NoError(t, err)
	_, err = client.httpClient.Do(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after 10 redirects")
	require.Equal(t, 10, calls)
}

func TestRedirectCustomPolicyStopsRequests(t *testing.T) {
	sentinel := errors.New("redirect rejected")
	for _, policyErr := range []error{sentinel, http.ErrUseLastResponse} {
		t.Run(policyErr.Error(), func(t *testing.T) {
			targetCalls := make(chan struct{}, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls <- struct{}{}
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusFound)
			}))
			defer source.Close()
			client := NewClient(source.URL, "tenant", "OPEN-SANDBOX-API-KEY",
				WithHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
					return policyErr
				}}))
			req, err := http.NewRequest(http.MethodGet, source.URL, nil)
			require.NoError(t, err)
			resp, err := client.httpClient.Do(req)
			if policyErr == http.ErrUseLastResponse {
				require.NoError(t, err)
				require.Equal(t, http.StatusFound, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
			} else {
				require.ErrorIs(t, err, sentinel)
			}
			require.Equal(t, 0, len(targetCalls))
		})
	}
}

func TestRedirectRequestPaths(t *testing.T) {
	for _, path := range []string{"lifecycle", "execd", "proxy-execd", "egress", "download", "upload", "SSE"} {
		for _, sameOrigin := range []bool{true, false} {
			name := path + "/cross-origin"
			if sameOrigin {
				name = path + "/same-origin"
			}
			t.Run(name, func(t *testing.T) {
				observed := make(chan http.Header, 4)
				targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					observed <- r.Header.Clone()
					if r.URL.Path != "/final" {
						// Another hop on the target origin must also be sanitized.
						http.Redirect(w, r, "/final", http.StatusFound)
						return
					}
					switch path {
					case "SSE":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: metrics\ndata: {}\n\n")
					case "download":
						_, _ = io.WriteString(w, "file content")
					default:
						_, _ = io.WriteString(w, "{}")
					}
				})
				target := httptest.NewServer(targetHandler)
				defer target.Close()
				destination := target.URL + "/hop"
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if sameOrigin && (r.URL.Path == "/hop" || r.URL.Path == "/final") {
						targetHandler.ServeHTTP(w, r)
						return
					}
					// Drain streaming multipart bodies before redirecting.
					_, _ = io.Copy(io.Discard, r.Body)
					http.Redirect(w, r, destination, http.StatusFound)
				}))
				defer source.Close()
				if sameOrigin {
					destination = source.URL + "/hop"
				}
				opts := []Option{WithHeaders(map[string]string{
					"OPEN-SANDBOX-API-KEY":      "tenant",
					"OpenSandbox-Secure-Access": "endpoint",
					"OPENSANDBOX-EGRESS-AUTH":   "egress",
					"X-Custom":                  "keep",
				})}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				authHeader := execdAuthHeader
				switch path {
				case "lifecycle":
					authHeader = "X-API-Key"
					opts = append(opts, WithAuthHeader(authHeader))
					client := NewLifecycleClient(source.URL, "tenant", opts...)
					err := client.doRequest(ctx, http.MethodGet, "/start", nil, nil)
					require.NoError(t, err)
				case "execd":
					require.NoError(t, NewExecdClient(source.URL, "execd-token", opts...).Ping(ctx))
				case "proxy-execd":
					// Proxy-mode data-plane requests retain the tenant key while
					// ConnectionConfig applies the lifecycle auth override to execd.
					cfg := ConnectionConfig{UseServerProxy: true, AuthHeader: "X-API-Key", Headers: map[string]string{
						"X-API-Key": "tenant", "X-Custom": "keep",
					}}
					client := cfg.execdClient(source.URL, map[string]string{
						execdAuthHeader: "endpoint-token", "OPEN-SANDBOX-API-KEY": "tenant",
						"OpenSandbox-Secure-Access": "endpoint", "OPENSANDBOX-EGRESS-AUTH": "egress",
					})
					require.NoError(t, client.Ping(ctx))
				case "egress":
					authHeader = egressAuthHeader
					_, err := NewEgressClient(source.URL, "egress", opts...).GetPolicy(ctx)
					require.NoError(t, err)
				case "download":
					reader, err := NewExecdClient(source.URL, "execd-token", opts...).DownloadFile(ctx, "/file", "")
					require.NoError(t, err)
					content, err := io.ReadAll(reader)
					require.NoError(t, err)
					require.NoError(t, reader.Close())
					require.Equal(t, "file content", string(content))
				case "upload":
					err := NewExecdClient(source.URL, "execd-token", opts...).UploadFile(ctx, strings.NewReader("upload"),
						UploadFileOptions{FileName: "file", Metadata: FileMetadata{Path: "/file"}})
					require.NoError(t, err)
				case "SSE":
					events := 0
					err := NewExecdClient(source.URL, "execd-token", opts...).WatchMetrics(ctx, func(event StreamEvent) error {
						events++
						return nil
					})
					require.NoError(t, err)
					require.Equal(t, 1, events)
				}
				require.Equal(t, 2, len(observed))
				for i := 0; i < 2; i++ {
					headers := <-observed
					require.Equal(t, "keep", headers.Get("X-Custom"))
					for _, name := range []string{"OPEN-SANDBOX-API-KEY", "OpenSandbox-Secure-Access", "OPENSANDBOX-EGRESS-AUTH", authHeader} {
						if sameOrigin {
							require.NotEmpty(t, headers.Get(name))
						} else {
							require.Equal(t, "", headers.Get(name))
						}
					}
					if path == "proxy-execd" {
						if sameOrigin {
							require.Equal(t, "tenant", headers.Get("X-API-Key"))
						} else {
							require.Equal(t, "", headers.Get("X-API-Key"))
						}
					}
				}
			})
		}
	}
}
