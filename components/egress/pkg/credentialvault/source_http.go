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

package credentialvault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const httpSourceDefaultTimeout = 10 * time.Second

func defaultTransportDialer() *net.Dialer {
	return &net.Dialer{Timeout: 5 * time.Second}
}

type httpSource struct {
	mu sync.RWMutex
	sf singleflight.Group

	initialURL     string
	initialMethod  string
	initialHeaders map[string]string

	nextURL        string
	nextHeaders    map[string]string
	headersRotated bool

	cachedValue string
	expiresAt   time.Time
	fetched     bool

	client *http.Client
}

type httpSourceResponse struct {
	Value   string            `json:"value"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	TTL     *int              `json:"ttl,omitempty"`
}

func (s *httpSource) Type() string { return SourceTypeHTTP }

func (s *httpSource) cachedValid() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fetched && (s.expiresAt.IsZero() || time.Now().Before(s.expiresAt)) {
		return s.cachedValue, true
	}
	return "", false
}

func (s *httpSource) Resolve(ctx context.Context) (string, error) {
	if val, ok := s.cachedValid(); ok {
		return val, nil
	}

	v, err, _ := s.sf.Do("fetch", func() (any, error) {
		if val, ok := s.cachedValid(); ok {
			return val, nil
		}
		return s.fetch(ctx)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (s *httpSource) fetch(ctx context.Context) (string, error) {
	s.mu.RLock()
	url := s.nextURL
	if url == "" {
		url = s.initialURL
	}
	var headers map[string]string
	if s.headersRotated {
		headers = s.nextHeaders
	} else {
		headers = s.initialHeaders
	}
	s.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, s.initialMethod, url, nil)
	if err != nil {
		return "", fmt.Errorf("http credential source: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http credential source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http credential source: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialVaultBodyBytes))
	if err != nil {
		return "", fmt.Errorf("http credential source: read body: %w", err)
	}

	var result httpSourceResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("http credential source: parse response: %w", err)
	}
	if result.Value == "" {
		return "", fmt.Errorf("http credential source: response value is empty")
	}

	s.mu.Lock()
	s.cachedValue = result.Value
	s.fetched = true
	if result.TTL != nil {
		s.expiresAt = time.Now().Add(time.Duration(*result.TTL) * time.Second)
	} else {
		s.expiresAt = time.Time{}
	}
	urlValid := result.URL == "" || validateHTTPSourceURL(result.URL) == nil
	headersValid := result.Headers == nil || validateHTTPSourceHeaders(result.Headers) == nil
	if urlValid && headersValid {
		// Determine whether the response actually rotates the URL. Providers
		// that echo the same refresh endpoint should not implicitly clear
		// bootstrap headers, otherwise the next fetch would go to the same
		// endpoint without auth headers. Compare canonical forms so an echo
		// that only differs in host case, an explicit default port, or a
		// fragment is not treated as a cross-endpoint rotation. Only an
		// explicit `headers: {}` (or a non-nil rotated map) may clear or
		// replace headers.
		currentURL := s.nextURL
		if currentURL == "" {
			currentURL = s.initialURL
		}
		urlChanged := result.URL != "" && canonicalHTTPSourceURL(result.URL) != canonicalHTTPSourceURL(currentURL)
		if urlChanged {
			s.nextURL = result.URL
			if result.Headers == nil {
				s.nextHeaders = nil
				s.headersRotated = true
			}
		}
		if result.Headers != nil {
			s.nextHeaders = result.Headers
			s.headersRotated = true
		}
	}
	s.mu.Unlock()

	return result.Value, nil
}

func httpSourceFactory(raw json.RawMessage) (CredentialSource, error) {
	var cfg struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Method  string            `json:"method,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse http credential source: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("http credential source url cannot be empty")
	}
	if err := validateHTTPSourceURL(cfg.URL); err != nil {
		return nil, err
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	if _, err := http.NewRequest(cfg.Method, cfg.URL, nil); err != nil {
		return nil, fmt.Errorf("http credential source: invalid method or url: %w", err)
	}
	if err := validateHTTPSourceHeaders(cfg.Headers); err != nil {
		return nil, err
	}
	return &httpSource{
		initialURL:     cfg.URL,
		initialMethod:  cfg.Method,
		initialHeaders: cfg.Headers,
		client: &http.Client{
			Timeout:   httpSourceDefaultTimeout,
			Transport: httpSourceTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errors.New("http credential source: redirects are not allowed")
			},
		},
	}, nil
}

func validateHTTPSourceHeaders(headers map[string]string) error {
	seen := make(map[string]string, len(headers))
	for name, value := range headers {
		if !headerFieldNamePattern.MatchString(name) {
			return fmt.Errorf("http credential source: invalid header name %q", name)
		}
		// Header names are case-insensitive. If two map entries collapse to
		// the same canonical name, Go map iteration order determines which
		// value wins after req.Header.Set, so provider auth would become
		// nondeterministic. Reject at write time instead.
		lower := strings.ToLower(name)
		if existing, ok := seen[lower]; ok {
			return fmt.Errorf("http credential source: duplicate header %q (also seen as %q)", name, existing)
		}
		seen[lower] = name
		for i := range value {
			b := value[i]
			if b < 0x20 || b == 0x7f {
				return fmt.Errorf("http credential source: header %q value contains invalid character 0x%02x", name, b)
			}
		}
	}
	return nil
}

func validateHTTPSourceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("http credential source url: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("http credential source url must use http or https scheme, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("http credential source url must include a host")
	}
	if port := parsed.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("http credential source url has invalid port %q", port)
		}
	}
	if isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("http credential source url must not target a loopback host %q; the sandbox nft output chain accepts loopback unconditionally, so such fetches bypass network policy", parsed.Hostname())
	}
	return nil
}

// isLoopbackHost reports whether host resolves to a loopback address. It
// covers IPv4/IPv6 literals (including 127.0.0.0/8 and ::1) and the special
// hostname "localhost". Zones are stripped from IPv6 literals before parsing.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// Strip an IPv6 zone identifier ("fe80::1%eth0") before net.ParseIP.
	if i := strings.Index(host, "%"); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// canonicalHTTPSourceURL returns a normalized form of a validated HTTP source
// URL that ignores case-insensitive scheme/host differences, an explicit
// default port for the scheme, and URL fragments, so that echoing the same
// endpoint in a different textual form does not look like a rotation.
func canonicalHTTPSourceURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = host + ":" + port
	} else {
		parsed.Host = host
	}
	parsed.Fragment = ""
	return parsed.String()
}
