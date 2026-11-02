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
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// Addon → egress resolve RPC (OSEP-0025 T2b). The addon owns binding
// evaluation on the active snapshot; this endpoint owns the per-request
// resolver call. Discipline: capture under the Store read lock, release
// before the resolver call (a slow resolver must never pin a Store lock),
// recheck by binding content fingerprint afterwards (D12) — a bare revision
// cannot detect a same-name replacement or a delete/recreate revision
// wraparound, and an unrelated mutation must not invalidate.

const maxResolveRequestBytes = 64 << 10

// ResolveAccess is the requesting sandbox's resolve authorization: the wire
// subject (the egress-authoritative sandboxId, D11) and its vault Store.
type ResolveAccess struct {
	SandboxID string
	Store     *Store
}

// ResolveAccessError is a fixed-vocabulary access failure.
type ResolveAccessError struct {
	Status int
	Reason string
}

func (e *ResolveAccessError) Error() string { return e.Reason }

// ResolveAccessFunc derives resolve access from the request. It runs once
// before the resolver call (capture) and once after it (recheck); the subject
// never comes from the protocol surface and the client IP never crosses the
// resolver boundary.
type ResolveAccessFunc func(r *http.Request) (ResolveAccess, *ResolveAccessError)

// ResolveHandler serves POST /credential-vault/_resolve.
type ResolveHandler struct {
	access    ResolveAccessFunc
	newClient func(endpoint string) (ResolverClient, error)
	audit     func(event ResolveAuditEvent)

	mu      sync.Mutex
	clients map[string]ResolverClient
}

// NewResolveHandler builds the resolve endpoint around one access derivation.
func NewResolveHandler(access ResolveAccessFunc) *ResolveHandler {
	return &ResolveHandler{
		access:    access,
		newClient: NewResolverClientFromEnv,
		audit:     defaultResolveAudit,
		clients:   make(map[string]ResolverClient),
	}
}

// ResolveAuditEvent is the R10 audit record: names, decision, and target host
// only — credential values never appear.
type ResolveAuditEvent struct {
	Resolution string // resolved | denied | timeout
	Credential string
	Binding    string
	TargetHost string
	Subject    string
	Duration   time.Duration
}

func defaultResolveAudit(event ResolveAuditEvent) {
	log.Infof("credential_resolve_audit resolution=%s credential=%s binding=%s target_host=%s subject=%s duration_ms=%d",
		event.Resolution, event.Credential, event.Binding, event.TargetHost, event.Subject, event.Duration.Milliseconds())
	telemetry.RecordCredentialResolution(event.Resolution, event.Duration.Seconds())
}

// ServeHTTP implements the §2 contract: strict JSON (no duplicate keys, exact
// field sets), exact Content-Type, bounded body, fixed error vocabulary, and
// the capture-release-recheck flow around the resolver call.
func (h *ResolveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		drainResolveBody(r)
		w.Header().Set("Allow", http.MethodPost)
		writeResolveError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if contentType := strings.TrimSpace(r.Header.Get("Content-Type")); contentType != "application/json" {
		drainResolveBody(r)
		writeResolveError(w, http.StatusBadRequest, "malformed request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxResolveRequestBytes+1))
	if err != nil {
		writeResolveError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if len(body) > maxResolveRequestBytes {
		drainResolveBody(r)
		writeResolveError(w, http.StatusBadRequest, "request too large")
		return
	}
	document, err := decodeResolveRequest(body)
	if err != nil {
		writeResolveError(w, http.StatusBadRequest, "malformed request")
		return
	}
	destination, frame, targetErr := buildResolveTarget(document)
	if targetErr != nil {
		writeResolveError(w, targetErr.status, targetErr.reason)
		return
	}

	started := time.Now()
	event := ResolveAuditEvent{Binding: document.Binding, TargetHost: frame.Host}
	outcome := telemetry.ResolutionOutcomeDenied
	defer func() {
		event.Resolution = outcome
		event.Duration = time.Since(started)
		h.audit(event)
	}()

	access, accessErr := h.access(r)
	if accessErr != nil {
		writeResolveError(w, accessErr.Status, accessErr.Reason)
		return
	}
	event.Subject = access.SandboxID
	if access.Store == nil {
		writeResolveError(w, http.StatusNotFound, "binding not found")
		return
	}
	capture, err := access.Store.CaptureResolve(document.Binding)
	if err != nil {
		writeResolveError(w, http.StatusNotFound, "binding not found")
		return
	}
	event.Credential = capture.Credential

	client, err := h.clientFor(capture.Endpoint)
	if err != nil {
		log.Warnf("credential resolver client unavailable: %v", err)
		writeResolveError(w, http.StatusServiceUnavailable, "resolver unavailable")
		return
	}
	decision, err := client.Resolve(r.Context(), ResolveInput{
		Subject:     access.SandboxID,
		Credential:  capture.Credential,
		Binding:     document.Binding,
		Destination: destination,
		Request:     frame,
	})
	if err != nil || decision == nil {
		if err == nil {
			err = resolverErrorf(ResolverUnavailable, "empty decision")
		}
		status, reason, timedOut := resolveFailureResponse(err)
		if timedOut {
			outcome = telemetry.ResolutionOutcomeTimeout
		}
		writeResolveError(w, status, reason)
		return
	}

	// Recheck (D12): the resolver call ran without any lock held. Any
	// identity change (subject replaced, store replaced) or captured-state
	// change (binding or credential content, vault deleted) discards the
	// resolved headers — never serve credentials resolved under state the
	// caller no longer owns.
	recheckAccess, accessErr := h.access(r)
	if accessErr != nil ||
		recheckAccess.SandboxID != access.SandboxID ||
		recheckAccess.Store != access.Store {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}
	recheck, err := recheckAccess.Store.CaptureResolve(document.Binding)
	if err != nil || recheck.Fingerprint != capture.Fingerprint {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}

	outcome = telemetry.ResolutionOutcomeResolved
	writeResolveSuccess(w, capture.Revision, document.Binding, decision)
}

// maxCachedResolverClients bounds the per-endpoint client cache: entries are
// never evicted (the handler lives for the process lifetime), so a churning
// endpoint set must not grow it without limit. Past the cap, clients are
// rebuilt per request — construction is parse-only, no I/O.
const maxCachedResolverClients = 64

// clientFor caches one ResolverClient per captured endpoint (hot resolvers
// are few); the env never changes at runtime.
func (h *ResolveHandler) clientFor(endpoint string) (ResolverClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if client, ok := h.clients[endpoint]; ok {
		return client, nil
	}
	client, err := h.newClient(endpoint)
	if err != nil {
		return nil, err
	}
	if len(h.clients) < maxCachedResolverClients {
		h.clients[endpoint] = client
	}
	return client, nil
}

// resolveFailureResponse maps T2a error classes to the §2 status table.
func resolveFailureResponse(err error) (status int, reason string, timedOut bool) {
	var resolverErr *ResolverError
	if !errors.As(err, &resolverErr) {
		return http.StatusServiceUnavailable, "resolver unavailable", false
	}
	switch resolverErr.Kind {
	case ResolverTimeout:
		return http.StatusGatewayTimeout, "resolver timeout", true
	case ResolverDenied:
		return http.StatusForbidden, "forbidden", false
	case ResolverInvalidResponse:
		return http.StatusBadRequest, "invalid resolver response", false
	case ResolverInvalidRequest:
		return http.StatusBadRequest, "malformed request", false
	default:
		return http.StatusServiceUnavailable, "resolver unavailable", false
	}
}

// ---------------------------------------------------------------------------
// Request decoding and target construction
// ---------------------------------------------------------------------------

type resolveEndpointRequest struct {
	Binding string
	SNI     string
	Request resolveEndpointHTTPRequest
}

type resolveEndpointHTTPRequest struct {
	Scheme string
	Host   string
	Port   int
	Method string
	Path   string
}

// decodeResolveRequest strictly decodes the addon request: exact field sets,
// unique keys, no trailing data, no null scalars.
func decodeResolveRequest(data []byte) (resolveEndpointRequest, error) {
	fields, err := strictJSONObject(data)
	if err != nil {
		return resolveEndpointRequest{}, err
	}
	if len(fields) != 3 {
		return resolveEndpointRequest{}, errors.New("resolve request requires exactly binding, sni, request")
	}
	document := resolveEndpointRequest{}
	if document.Binding, err = decodeStrictStringField(fields, "binding"); err != nil {
		return resolveEndpointRequest{}, err
	}
	if document.SNI, err = decodeStrictStringField(fields, "sni"); err != nil {
		return resolveEndpointRequest{}, err
	}
	requestRaw, ok := fields["request"]
	if !ok {
		return resolveEndpointRequest{}, errors.New("request field required")
	}
	requestFields, err := strictJSONObject(requestRaw)
	if err != nil {
		return resolveEndpointRequest{}, err
	}
	for name := range requestFields {
		if name != "scheme" && name != "host" && name != "port" && name != "method" && name != "path" {
			return resolveEndpointRequest{}, errors.New("request carries an unknown field")
		}
	}
	if len(requestFields) < 4 {
		return resolveEndpointRequest{}, errors.New("request requires scheme, host, method, path and optional port")
	}
	if document.Request.Scheme, err = decodeStrictStringField(requestFields, "scheme"); err != nil {
		return resolveEndpointRequest{}, err
	}
	if document.Request.Host, err = decodeStrictStringField(requestFields, "host"); err != nil {
		return resolveEndpointRequest{}, err
	}
	if document.Request.Method, err = decodeStrictStringField(requestFields, "method"); err != nil {
		return resolveEndpointRequest{}, err
	}
	if document.Request.Path, err = decodeStrictStringField(requestFields, "path"); err != nil {
		return resolveEndpointRequest{}, err
	}
	if portRaw, ok := requestFields["port"]; ok {
		trimmed := bytes.TrimSpace(portRaw)
		// JSON null unmarshals into an int as a no-op success; reject it
		// explicitly like every other scalar (no null scalars).
		if bytes.Equal(trimmed, []byte("null")) {
			return resolveEndpointRequest{}, errors.New("port must be an integer")
		}
		if err := json.Unmarshal(trimmed, &document.Request.Port); err != nil {
			return resolveEndpointRequest{}, errors.New("port must be an integer")
		}
	}
	return document, nil
}

func decodeStrictStringField(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("%s field required", name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("%s must be a string", name)
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

// buildResolveTarget validates and normalizes the outbound frame and applies
// the SNI gate: the resolver's proof binds to the destination, so the
// client-visible host must equal the connection's SNI (403 otherwise).
func buildResolveTarget(document resolveEndpointRequest) (ResolveDestination, ResolveRequestFrame, *resolveRequestError) {
	badRequest := &resolveRequestError{status: http.StatusBadRequest, reason: "malformed request"}
	forbidden := &resolveRequestError{status: http.StatusForbidden, reason: "forbidden"}

	binding := strings.TrimSpace(document.Binding)
	scheme := strings.ToLower(strings.TrimSpace(document.Request.Scheme))
	if scheme != "http" && scheme != "https" {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	port := document.Request.Port
	if port == 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	if port < 1 || port > 65535 {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	host, err := normalizeCredentialHost(strings.TrimSpace(document.Request.Host))
	if err != nil {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	sni, err := normalizeCredentialHost(strings.TrimSpace(document.SNI))
	if err != nil {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	if host != sni {
		return ResolveDestination{}, ResolveRequestFrame{}, forbidden
	}
	method := strings.ToUpper(strings.TrimSpace(document.Request.Method))
	path := strings.TrimSpace(document.Request.Path)
	if binding == "" || method == "" || !strings.HasPrefix(path, "/") {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	return ResolveDestination{
			Scheme:    scheme,
			Authority: host + ":" + strconv.Itoa(port),
			SNI:       sni,
		}, ResolveRequestFrame{
			Method:  method,
			Host:    host,
			Path:    path,
			Headers: map[string]string{},
		}, nil
}

// resolveRequestError is a fixed-vocabulary request-shape failure.
type resolveRequestError struct {
	status int
	reason string
}

func drainResolveBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxResolveRequestBytes+1))
}

// ---------------------------------------------------------------------------
// Response encoding
// ---------------------------------------------------------------------------

type resolveResponseHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resolveEndpointResponse struct {
	Revision        int64                   `json:"revision"`
	Binding         string                  `json:"binding"`
	Headers         []resolveResponseHeader `json:"headers"`
	Redactions      []string                `json:"redactions"`
	SuppressHeaders []string                `json:"suppressHeaders"`
}

func writeResolveSuccess(w http.ResponseWriter, revision int64, binding string, decision *ResolverDecision) {
	names := make([]string, 0, len(decision.Headers))
	for name := range decision.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := make([]resolveResponseHeader, 0, len(names))
	suppress := make([]string, 0, len(decision.Suppress))
	for name := range decision.Suppress {
		suppress = append(suppress, name)
	}
	sort.Strings(suppress)
	for _, name := range names {
		headers = append(headers, resolveResponseHeader{Name: name, Value: decision.Headers[name]})
	}
	redactions := decision.Redactions
	if redactions == nil {
		redactions = []string{}
	}
	writeResolveJSON(w, http.StatusOK, resolveEndpointResponse{
		Revision:        revision,
		Binding:         binding,
		Headers:         headers,
		Redactions:      redactions,
		SuppressHeaders: suppress,
	})
}

func writeResolveError(w http.ResponseWriter, status int, reason string) {
	http.Error(w, reason, status)
}

func writeResolveJSON(w http.ResponseWriter, status int, document resolveEndpointResponse) {
	encoded, err := json.Marshal(document)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// ---------------------------------------------------------------------------
// Store capture (D12)
// ---------------------------------------------------------------------------

// ResolveCapture is the immutable per-binding resolve state captured under
// the Store read lock and released before the resolver call. Fingerprint is
// over the binding content and the referenced credential's exact source
// object, so a same-name replacement — including a delete/recreate whose
// revision wraps back — with different content is always detectable, while an
// unrelated mutation that did not touch the captured binding does not
// invalidate (OSEP-0012 immutable-snapshot semantics). The active-snapshot
// tag is deliberately NOT captured: it flips on unrelated mutations and must
// not gate invalidation.
type ResolveCapture struct {
	Credential  string
	Endpoint    string
	Revision    int64
	Fingerprint [sha256.Size]byte
}

// CaptureResolve captures the exchange binding's resolve inputs: the
// referenced exchange credential's resolver endpoint plus the content
// fingerprint. Non-exchange bindings, unknown names, and missing vaults are
// not resolvable (ErrNotFound).
func (v *Store) CaptureResolve(bindingName string) (ResolveCapture, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists {
		return ResolveCapture{}, ErrNotFound
	}
	binding, ok := v.bindings[bindingName]
	if !ok || binding.Auth.Type != "" {
		return ResolveCapture{}, ErrNotFound
	}
	credential, ok := v.credentials[binding.Auth.Credential]
	if !ok || credential.SourceType != "exchange" {
		return ResolveCapture{}, ErrNotFound
	}
	exchange, ok := credential.Source.(*exchangeSource)
	if !ok {
		return ResolveCapture{}, ErrNotFound
	}
	return ResolveCapture{
		Credential:  credential.Name,
		Endpoint:    exchange.endpoint,
		Revision:    v.revision,
		Fingerprint: resolveFingerprint(binding, credential.raw),
	}, nil
}

// resolveFingerprint digests the binding and credential source content. The
// comparison happens outside any lock; the digest itself is computed under
// the capturing read lock so it names the exact captured state.
func resolveFingerprint(binding Binding, credentialRaw json.RawMessage) [sha256.Size]byte {
	bindingBytes, _ := json.Marshal(binding)
	h := sha256.New()
	writeField := func(label string, value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write([]byte(label))
		_, _ = h.Write(length[:])
		_, _ = h.Write(value)
	}
	writeField("binding", bindingBytes)
	writeField("credential", credentialRaw)
	return [sha256.Size]byte(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Resolver capability gate wiring (D13)
// ---------------------------------------------------------------------------

// ResolverConfigFromEnv derives the exchange capability gate from the
// operator env. A malformed timeout or auth-table value fails its flag closed
// (exchange vaults are then rejected at admission); absent values keep the
// documented defaults and count as ready. The returned error is diagnostic
// only — the config itself is authoritative.
func ResolverConfigFromEnv() (ResolverConfig, error) {
	config := ResolverConfig{TimeoutConfigured: true, AuthTableParsed: true}
	var diag error
	if raw := strings.TrimSpace(os.Getenv(constants.EnvCredentialResolverTimeout)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 120 {
			config.TimeoutConfigured = false
			diag = fmt.Errorf("%s value is not an integer in [1,120]", constants.EnvCredentialResolverTimeout)
		}
	}
	if _, err := ParseResolverAuthEnv(os.Getenv(constants.EnvCredentialResolverAuth)); err != nil {
		config.AuthTableParsed = false
		if diag == nil {
			diag = err
		}
	}
	return config, diag
}

// NewSubjectStore creates a per-subject store (fast-sandbox profile) with
// the resolver capability gate derived from the operator env, so a reloaded
// exchange vault is admitted under the same D13 gate as a fresh write.
func NewSubjectStore() *Store {
	store := NewStore(nil, func() bool { return true })
	config, err := ResolverConfigFromEnv()
	if err != nil {
		log.Warnf("credential resolver operator configuration not ready: %v", err)
	}
	store.SetResolverConfig(config)
	return store
}
