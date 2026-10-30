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

package credentialvault

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// Egress → credential header resolver client (OSEP-0025, frozen contract
// §3.3): one POSTed JSON request document answered with a header map plus
// optional incremental redactions. The envelope is untrusted input (D14):
// every response over a limit is a per-request denial, never a global fault.

const (
	// maxResolverResponseBytes (D14): total response body byte cap.
	maxResolverResponseBytes = 64 << 10
	// maxResolverHeaderCount (D14): header entries per response.
	maxResolverHeaderCount = 16
	// maxResolverValueBytes (D14): single header name/value or redaction value cap.
	maxResolverValueBytes = 8 << 10
	// maxResolverRedactionCount (D14): total resolver-provided redaction values.
	maxResolverRedactionCount = 64
	// maxResolverResponseHeaderBytes caps the HTTP response header block itself.
	maxResolverResponseHeaderBytes = 8 << 10
)

// ResolverErrorKind classifies a failed resolve for the interception path.
// Denied/invalid/rejected/timeout/unavailable are all per-request outcomes:
// the request proceeds without resolver headers (fail closed), the client
// itself stays healthy.
type ResolverErrorKind string

const (
	// ResolverInvalidRequest: caller-supplied input violates the request contract.
	ResolverInvalidRequest ResolverErrorKind = "invalid_request"
	// ResolverUnavailable: transport failure (dial/write/read error).
	ResolverUnavailable ResolverErrorKind = "unavailable"
	// ResolverTimeout: per-call deadline exceeded.
	ResolverTimeout ResolverErrorKind = "timeout"
	// ResolverRejected: resolver answered with a non-200 status.
	ResolverRejected ResolverErrorKind = "rejected"
	// ResolverInvalidResponse: 200 but the body is not the contracted JSON envelope.
	ResolverInvalidResponse ResolverErrorKind = "invalid_response"
	// ResolverDenied (D14/R14): envelope limit exceeded or header name denied —
	// per-request denial.
	ResolverDenied ResolverErrorKind = "denied"
)

// ResolverError is the typed failure of one resolve call.
type ResolverError struct {
	Kind   ResolverErrorKind
	Reason string
	Status int
	err    error
}

func (e *ResolverError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "resolver %s", e.Kind)
	if e.Reason != "" {
		fmt.Fprintf(&b, " (%s)", e.Reason)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " status=%d", e.Status)
	}
	return b.String()
}

func (e *ResolverError) Unwrap() error { return e.err }

func resolverErrorf(kind ResolverErrorKind, reason string) *ResolverError {
	return &ResolverError{Kind: kind, Reason: reason}
}

// ResolverClient resolves one HTTP exchange's credential headers against the
// credential header resolver.
type ResolverClient interface {
	Resolve(ctx context.Context, input ResolveInput) (*ResolverDecision, error)
}

// ResolveInput is one resolution request (frozen contract request document).
type ResolveInput struct {
	// Subject is the egress-authoritative sandboxId.
	Subject string
	// Credential and Binding name the vault credential and binding whose
	// headers the resolver renders.
	Credential string
	Binding    string
	// Destination is the target origin of the intercepted exchange.
	Destination ResolveDestination
	// Request describes the outbound request frame the headers apply to.
	Request ResolveRequestFrame
}

// ResolveDestination identifies the intercepted origin.
type ResolveDestination struct {
	Scheme    string // "http" or "https"
	Authority string // host:port of the origin
	SNI       string // TLS server name (authority host for https)
}

// ResolveRequestFrame describes the outbound request.
type ResolveRequestFrame struct {
	Method  string
	Host    string
	Path    string // starts with "/"
	Headers map[string]string
}

// ResolverDecision is the validated per-request outcome: headers to inject,
// the header names to suppress from passthrough, and the merged redaction
// set (derived variants are authoritative; resolver-provided redactions are
// incremental only, R9).
type ResolverDecision struct {
	Headers    map[string]string
	Suppress   map[string]struct{} // lowercase header names
	Redactions []string            // canonically ordered, deduped, non-empty
}

// ResolverClientOptions constructs one client.
type ResolverClientOptions struct {
	// Endpoint is the resolver URI: "uds:///absolute/socket/path" or
	// "unix://..." (alias) or "https://host[:port][/prefix]".
	Endpoint string
	// Timeout bounds every resolve call; 0 selects the configured default.
	Timeout time.Duration
	// MaxConcurrency bounds in-flight resolves; 0 selects the configured default.
	MaxConcurrency int
	// Auth maps normalized origins ("https://host:port") to the bearer token
	// presented for that origin only (D5). Endpoints without a matching
	// origin resolve without an Authorization header.
	Auth map[string]string
	// TLSClientConfig overrides the HTTPS transport TLS configuration (test
	// and deployment seam). Nil selects system verification.
	TLSClientConfig *tls.Config
}

// NewResolverClient validates the endpoint and constructs the transport
// without dialing.
func NewResolverClient(options ResolverClientOptions) (ResolverClient, error) {
	if options.Timeout <= 0 {
		options.Timeout = ResolverTimeoutFromEnv()
	}
	if options.Timeout <= 0 {
		return nil, resolverErrorf(ResolverInvalidRequest, "timeout_must_be_positive")
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = ResolverMaxConcurrencyFromEnv()
	}
	if options.MaxConcurrency <= 0 {
		return nil, resolverErrorf(ResolverInvalidRequest, "max_concurrency_must_be_positive")
	}
	parsed, err := parseResolverEndpoint(options.Endpoint)
	if err != nil {
		return nil, err
	}

	client := &resolverClient{
		timeout:     options.Timeout,
		concurrency: make(chan struct{}, options.MaxConcurrency),
	}
	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var transport *http.Transport
	if parsed.network == "unix" {
		socketPath := parsed.socketPath
		dialer := &net.Dialer{}
		transport = &http.Transport{
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxResolverResponseHeaderBytes,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		client.requestURL = (&url.URL{Scheme: "http", Host: "localhost", Path: parsed.resolvePath}).String()
	} else {
		if parsed.origin == "" {
			return nil, resolverErrorf(ResolverInvalidRequest, "missing_https_origin")
		}
		// Proxy stays nil: the resolver hop is direct TLS and must never
		// chain through an outbound proxy (the bearer token must not leak
		// to one).
		transport = &http.Transport{
			DisableKeepAlives:      true,
			ForceAttemptHTTP2:      false,
			MaxResponseHeaderBytes: maxResolverResponseHeaderBytes,
			TLSClientConfig:        options.TLSClientConfig,
		}
		client.requestURL = (&url.URL{Scheme: "https", Host: parsed.authority, Path: parsed.resolvePath}).String()
		client.hostHeader = parsed.authority
		// D5: origin-scoped bearer token; an unmatched origin sends no header.
		if token, ok := options.Auth[parsed.origin]; ok {
			client.authorization = "Bearer " + token
		}
	}
	httpClient.Transport = transport
	client.http = httpClient
	return client, nil
}

type parsedResolverEndpoint struct {
	network     string // "unix" or "tcp"
	socketPath  string // unix only
	authority   string // https host[:port] as configured
	origin      string // normalized "https://host:port" (D5 lookup key)
	resolvePath string // request target: prefix + "/resolve"
}

// parseResolverEndpoint validates the resolver URI and derives the framing
// (D15): https endpoints may carry a path prefix; the resolve target is
// prefix + "/resolve". Unix endpoints carry no prefix.
func parseResolverEndpoint(endpoint string) (*parsedResolverEndpoint, error) {
	raw, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("invalid resolver endpoint %q: %w", endpoint, err)
	}
	switch strings.ToLower(raw.Scheme) {
	case "uds", "unix":
		if raw.Host != "" {
			return nil, resolverErrorf(ResolverInvalidRequest, "uds_endpoint_requires_empty_authority")
		}
		if !filepath.IsAbs(raw.Path) {
			return nil, resolverErrorf(ResolverInvalidRequest, "uds_endpoint_requires_absolute_path")
		}
		return &parsedResolverEndpoint{
			network:     "unix",
			socketPath:  filepath.Clean(raw.Path),
			resolvePath: "/resolve",
		}, nil
	case "https":
		if raw.Host == "" {
			return nil, resolverErrorf(ResolverInvalidRequest, "https_endpoint_requires_authority")
		}
		if raw.RawQuery != "" || raw.Fragment != "" || raw.User != nil {
			return nil, resolverErrorf(ResolverInvalidRequest, "https_endpoint_rejects_query_fragment_userinfo")
		}
		origin, err := normalizeResolverOrigin(raw)
		if err != nil {
			return nil, err
		}
		prefix := raw.Path
		if prefix == "/" {
			prefix = ""
		}
		if prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
			return nil, resolverErrorf(ResolverInvalidRequest, "https_endpoint_prefix_must_be_a_non_trailing_slash_path")
		}
		return &parsedResolverEndpoint{
			network:     "tcp",
			authority:   raw.Host,
			origin:      origin,
			resolvePath: prefix + "/resolve",
		}, nil
	default:
		return nil, resolverErrorf(ResolverInvalidRequest, "unsupported_endpoint_scheme")
	}
}

// normalizeResolverOrigin canonicalizes an https URL to "https://host:port"
// (D5): lowercase scheme and host, explicit port (443 added when absent).
func normalizeResolverOrigin(u *url.URL) (string, error) {
	if strings.ToLower(u.Scheme) != "https" {
		return "", resolverErrorf(ResolverInvalidRequest, "origin_requires_https_scheme")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", resolverErrorf(ResolverInvalidRequest, "origin_requires_host")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host + ":" + port, nil
}

// ParseResolverAuthEnv parses OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_AUTH
// (D5): comma-separated "origin=token" entries. Origins are normalized; a
// malformed entry is a configuration error (fail closed), and an empty token
// is rejected rather than treated as "no auth".
func ParseResolverAuthEnv(raw string) (map[string]string, error) {
	tokens := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		origin, token, found := strings.Cut(entry, "=")
		origin = strings.TrimSpace(origin)
		token = strings.TrimSpace(token)
		if !found || origin == "" || token == "" {
			return nil, fmt.Errorf("invalid resolver auth entry (want origin=token)")
		}
		normalized, err := normalizeResolverOriginString(origin)
		if err != nil {
			return nil, fmt.Errorf("invalid resolver auth origin %q: %w", origin, err)
		}
		if _, duplicate := tokens[normalized]; duplicate {
			return nil, fmt.Errorf("duplicate resolver auth origin %q", origin)
		}
		tokens[normalized] = token
	}
	return tokens, nil
}

func normalizeResolverOriginString(origin string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return "", err
	}
	return normalizeResolverOrigin(parsed)
}

// ResolverTimeoutFromEnv reads OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_TIMEOUT
// (seconds); unparseable or out-of-range values keep the default.
func ResolverTimeoutFromEnv() time.Duration {
	seconds := constants.EnvIntRange(
		constants.EnvCredentialResolverTimeout,
		constants.DefaultCredentialResolverTimeoutSeconds,
		1, 120,
	)
	return time.Duration(seconds) * time.Second
}

// ResolverMaxConcurrencyFromEnv reads
// OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_MAX_CONCURRENCY; unparseable or
// out-of-range values keep the default.
func ResolverMaxConcurrencyFromEnv() int {
	return constants.EnvIntRange(
		constants.EnvCredentialResolverMaxConcurrency,
		constants.DefaultCredentialResolverMaxConcurrency,
		1, 1024,
	)
}

// NewResolverClientFromEnv constructs a client from the three resolver
// environment variables (endpoint supplied by the caller — there is no
// endpoint env; wiring owns it).
func NewResolverClientFromEnv(endpoint string) (ResolverClient, error) {
	auth, err := authTokensFromEnv()
	if err != nil {
		return nil, err
	}
	return NewResolverClient(ResolverClientOptions{
		Endpoint: endpoint,
		Auth:     auth,
	})
}

func authTokensFromEnv() (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv(constants.EnvCredentialResolverAuth))
	if raw == "" {
		return nil, nil
	}
	return ParseResolverAuthEnv(raw)
}

// resolverClient is the UDS/HTTPS resolver transport. The zero value is not
// usable; construct through NewResolverClient.
type resolverClient struct {
	http          *http.Client
	requestURL    string
	hostHeader    string // https: endpoint authority; empty for unix (localhost)
	authorization string // D5: "Bearer <token>" only when the origin matched
	timeout       time.Duration
	concurrency   chan struct{}
}

var _ ResolverClient = (*resolverClient)(nil)

// Resolve performs one resolution. Failures are typed ResolverError values;
// envelope violations (D14) are per-request denials.
func (c *resolverClient) Resolve(ctx context.Context, input ResolveInput) (*ResolverDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, &ResolverError{Kind: ResolverTimeout, err: err}
	}
	document, err := marshalResolveInput(input)
	if err != nil {
		return nil, err
	}

	// Bound in-flight resolves; the acquire honors caller cancellation.
	select {
	case c.concurrency <- struct{}{}:
		defer func() { <-c.concurrency }()
	case <-ctx.Done():
		return nil, &ResolverError{Kind: ResolverTimeout, err: ctx.Err()}
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.requestURL, bytes.NewReader(document))
	if err != nil {
		return nil, resolverErrorf(ResolverInvalidRequest, "request_build_failed")
	}
	request.Header.Set("Content-Type", "application/json")
	if c.hostHeader != "" {
		// D15: Host is the endpoint authority (https) or localhost (unix,
		// applied by the request URL above).
		request.Host = c.hostHeader
	}
	if c.authorization != "" {
		request.Header.Set("Authorization", c.authorization)
	}
	request.Close = true

	response, err := c.http.Do(request)
	if err != nil {
		if callCtx.Err() != nil {
			return nil, &ResolverError{Kind: ResolverTimeout, err: callCtx.Err()}
		}
		return nil, &ResolverError{Kind: ResolverUnavailable, err: err}
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reclaimed without letting
		// the peer stream unbounded bytes into the client.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResolverResponseHeaderBytes))
		return nil, &ResolverError{Kind: ResolverRejected, Status: response.StatusCode}
	}
	if mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return nil, resolverErrorf(ResolverInvalidResponse, "content_type_not_json")
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxResolverResponseBytes+1))
	if err != nil {
		return nil, &ResolverError{Kind: ResolverUnavailable, err: err}
	}
	if len(data) > maxResolverResponseBytes {
		return nil, resolverErrorf(ResolverDenied, "response_too_large")
	}
	envelope, err := decodeResolveResponse(data)
	if err != nil {
		return nil, err
	}
	return deriveResolverDecision(envelope.headers, envelope.redactions), nil
}

// marshalResolveInput validates the request-side contract (mirror of the
// resolver's own validation) and encodes the exact request document.
func marshalResolveInput(input ResolveInput) ([]byte, error) {
	nonEmpty := func(value string) bool { return value != "" }
	switch {
	case !nonEmpty(input.Subject), !nonEmpty(input.Credential), !nonEmpty(input.Binding):
		return nil, resolverErrorf(ResolverInvalidRequest, "subject_credential_binding_required")
	case input.Destination.Scheme != "http" && input.Destination.Scheme != "https":
		return nil, resolverErrorf(ResolverInvalidRequest, "destination_scheme_invalid")
	case !nonEmpty(input.Destination.Authority), !nonEmpty(input.Destination.SNI):
		return nil, resolverErrorf(ResolverInvalidRequest, "destination_authority_sni_required")
	case !nonEmpty(input.Request.Method), !nonEmpty(input.Request.Host):
		return nil, resolverErrorf(ResolverInvalidRequest, "request_method_host_required")
	case !strings.HasPrefix(input.Request.Path, "/"):
		return nil, resolverErrorf(ResolverInvalidRequest, "request_path_must_start_with_slash")
	}
	for name, value := range input.Request.Headers {
		if !nonEmpty(name) || !nonEmpty(value) {
			return nil, resolverErrorf(ResolverInvalidRequest, "request_headers_must_be_non_empty_strings")
		}
	}
	document := resolveRequestDocument{
		Subject:    input.Subject,
		Credential: input.Credential,
		Binding:    input.Binding,
		Destination: resolveDestinationDocument{
			Scheme:    input.Destination.Scheme,
			Authority: input.Destination.Authority,
			SNI:       input.Destination.SNI,
		},
		Request: resolveRequestFrameDocument{
			Method:  input.Request.Method,
			Host:    input.Request.Host,
			Path:    input.Request.Path,
			Headers: input.Request.Headers,
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, resolverErrorf(ResolverInvalidRequest, "request_encode_failed")
	}
	if len(encoded) > maxResolverResponseBytes {
		return nil, resolverErrorf(ResolverInvalidRequest, "request_too_large")
	}
	return encoded, nil
}

type resolveRequestDocument struct {
	Subject     string                      `json:"subject"`
	Credential  string                      `json:"credential"`
	Binding     string                      `json:"binding"`
	Destination resolveDestinationDocument  `json:"destination"`
	Request     resolveRequestFrameDocument `json:"request"`
}

type resolveDestinationDocument struct {
	Scheme    string `json:"scheme"`
	Authority string `json:"authority"`
	SNI       string `json:"sni"`
}

type resolveRequestFrameDocument struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

// decodeResolveResponse strictly decodes the response envelope: exactly the
// contracted fields, no duplicate keys, no unknown fields, no nulls, no
// trailing data (D14). Limit violations are ResolverDenied; schema violations
// are ResolverInvalidResponse.
func decodeResolveResponse(data []byte) (*resolveResponseDocument, error) {
	fields, err := strictJSONObject(data)
	if err != nil {
		return nil, resolverErrorf(ResolverInvalidResponse, "malformed_json")
	}
	for name := range fields {
		if name != "headers" && name != "redactions" {
			return nil, resolverErrorf(ResolverInvalidResponse, "unknown_field")
		}
	}
	headersRaw, ok := fields["headers"]
	if !ok {
		return nil, resolverErrorf(ResolverInvalidResponse, "headers_field_required")
	}
	if len(headersRaw) > maxResolverResponseBytes {
		return nil, resolverErrorf(ResolverDenied, "response_too_large")
	}
	document := &resolveResponseDocument{}
	if document.headers, err = decodeHeaderMap(headersRaw); err != nil {
		return nil, err
	}
	if redactionsRaw, ok := fields["redactions"]; ok {
		if document.redactions, err = decodeRedactionMap(redactionsRaw); err != nil {
			return nil, err
		}
	}
	return document, nil
}

type resolveResponseDocument struct {
	headers    map[string]string
	redactions map[string][]string
}

// decodeHeaderMap validates one resolved header map (D14/R14): names pass the
// credential-header rules (token pattern + reserved/hop-by-hop blacklist,
// vault.go reservedHeaderNames), values are non-empty strings within the
// per-value cap, and the map stays within the entry cap.
func decodeHeaderMap(raw json.RawMessage) (map[string]string, error) {
	fields, err := strictJSONObject(raw)
	if err != nil {
		return nil, resolverErrorf(ResolverInvalidResponse, "headers_not_object")
	}
	if len(fields) > maxResolverHeaderCount {
		return nil, resolverErrorf(ResolverDenied, "too_many_headers")
	}
	headers := make(map[string]string, len(fields))
	for name, valueRaw := range fields {
		if len(name) > maxResolverValueBytes {
			return nil, resolverErrorf(ResolverDenied, "header_name_too_long")
		}
		if validateCredentialHeaderName(name) != nil {
			return nil, resolverErrorf(ResolverDenied, "denied_header_name")
		}
		value, err := decodeNonEmptyString(valueRaw, "header_value_too_long")
		if err != nil {
			return nil, err
		}
		headers[name] = value
	}
	return headers, nil
}

// decodeRedactionMap validates the resolver-provided incremental redactions
// (R14): total values within the redaction cap, each value non-empty and
// within the per-value cap.
func decodeRedactionMap(raw json.RawMessage) (map[string][]string, error) {
	fields, err := strictJSONObject(raw)
	if err != nil {
		return nil, resolverErrorf(ResolverInvalidResponse, "redactions_not_object")
	}
	redactions := make(map[string][]string, len(fields))
	total := 0
	for name, valuesRaw := range fields {
		values, err := decodeNonEmptyStringArray(valuesRaw)
		if err != nil {
			return nil, err
		}
		total += len(values)
		if total > maxResolverRedactionCount {
			return nil, resolverErrorf(ResolverDenied, "too_many_redactions")
		}
		for _, value := range values {
			if len(value) > maxResolverValueBytes {
				return nil, resolverErrorf(ResolverDenied, "redaction_value_too_long")
			}
		}
		redactions[name] = values
	}
	return redactions, nil
}

func decodeNonEmptyString(raw json.RawMessage, tooLongReason string) (string, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '"' {
		return "", resolverErrorf(ResolverInvalidResponse, "value_not_string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", resolverErrorf(ResolverInvalidResponse, "value_not_string")
	}
	if value == "" {
		return "", resolverErrorf(ResolverDenied, "empty_value")
	}
	if len(value) > maxResolverValueBytes {
		return "", resolverErrorf(ResolverDenied, tooLongReason)
	}
	return value, nil
}

func decodeNonEmptyStringArray(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, resolverErrorf(ResolverInvalidResponse, "redaction_values_not_array")
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, resolverErrorf(ResolverInvalidResponse, "redaction_values_not_strings")
	}
	if len(values) == 0 {
		return nil, resolverErrorf(ResolverInvalidResponse, "redaction_values_empty")
	}
	for _, value := range values {
		if value == "" {
			return nil, resolverErrorf(ResolverInvalidResponse, "redaction_values_not_strings")
		}
		if len(value) > maxResolverValueBytes {
			return nil, resolverErrorf(ResolverDenied, "redaction_value_too_long")
		}
	}
	return values, nil
}

// strictJSONObject decodes exactly one JSON object with unique keys into
// name → raw value. Duplicate keys, non-objects, or trailing bytes are
// rejected.
func strictJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("not an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("non-string object key")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate key %q", name)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	if token, err = decoder.Token(); err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return nil, errors.New("unterminated object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return fields, nil
}

// canonicalResolverRedactions orders redaction values the way the decision
// snapshot expects: non-empty, deduped, ascending length then bytes.
func canonicalResolverRedactions(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	merged := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	sort.Slice(merged, func(i, j int) bool {
		if len(merged[i]) != len(merged[j]) {
			return len(merged[i]) < len(merged[j])
		}
		return merged[i] < merged[j]
	})
	return merged
}
