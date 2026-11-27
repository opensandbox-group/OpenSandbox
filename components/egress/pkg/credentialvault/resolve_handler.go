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

	"golang.org/x/net/http/httpguts"

	slogger "github.com/alibaba/opensandbox/internal/logger"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// Addon → egress resolve RPC (OSEP-0025): the addon owns binding evaluation,
// this endpoint owns the per-request resolver call. Capture under the Store
// read lock, release before the resolver call, recheck by binding content
// fingerprint (D12) — a bare revision misses same-name replacement and
// delete/recreate wraparound, and unrelated mutations must not invalidate.

const maxResolveRequestBytes = 64 << 10

// ResolveAccess is the requesting sandbox's resolve access: the wire subject
// (sandboxId, D11) and its vault Store.
type ResolveAccess struct {
	SandboxID string
	Store     *Store
	// ExpectedRevision fences a live request to its pinned decision snapshot.
	ExpectedRevision *int64
}

// ResolveAccessError is a fixed-vocabulary access failure.
type ResolveAccessError struct {
	Status int
	Reason string
}

func (e *ResolveAccessError) Error() string { return e.Reason }

// ResolveAccessFunc derives resolve access from the request; it runs at
// capture and again at recheck. The subject never comes from the protocol;
// the client IP never crosses the resolver boundary.
type ResolveAccessFunc func(r *http.Request) (ResolveAccess, *ResolveAccessError)

// ResolveHandler serves POST /credential-vault/_resolve.
type ResolveHandler struct {
	access    ResolveAccessFunc
	newClient func(endpoint string) (ResolverClient, error)
	audit     func(event ResolveAuditEvent)
	// The handler semaphore is the process-wide resolve bound: past the
	// client-cache cap each request builds its own client, and per-client
	// semaphores would no longer bound anything. timeout bounds queueing
	// and the call (T2a semantics).
	timeout   time.Duration
	semaphore chan struct{}

	mu      sync.Mutex
	clients map[string]ResolverClient
}

// NewResolveHandler builds the resolve endpoint around one access derivation.
func NewResolveHandler(access ResolveAccessFunc) *ResolveHandler {
	return &ResolveHandler{
		access:    access,
		newClient: NewResolverClientFromEnv,
		audit:     defaultResolveAudit,
		timeout:   ResolverTimeoutFromEnv(),
		semaphore: make(chan struct{}, ResolverMaxConcurrencyFromEnv()),
		clients:   make(map[string]ResolverClient),
	}
}

// ResolveAuditEvent is the R10 audit record: names, decision and target host
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
	log.Logger.With(
		slogger.Field{Key: "opensandbox.event", Value: "egress.credential.resolve"},
		slogger.Field{Key: "resolution", Value: event.Resolution},
		slogger.Field{Key: "credential", Value: event.Credential},
		slogger.Field{Key: "binding", Value: event.Binding},
		slogger.Field{Key: "target.host", Value: event.TargetHost},
		slogger.Field{Key: "subject", Value: event.Subject},
		slogger.Field{Key: "duration_ms", Value: event.Duration.Milliseconds()},
	).Infof("credential resolve audit")
	telemetry.RecordCredentialResolution(event.Resolution, event.Duration.Seconds())
}

// ServeHTTP serves the OSEP-0025 resolve contract: strict JSON, exact
// Content-Type, bounded body, fixed error vocabulary, capture-release-
// recheck.
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
	if len(document.Bindings) > 0 {
		h.serveGroup(w, r, document)
		return
	}

	// Audit starts after decoding but before target validation: a decoded
	// request that is then denied (SNI gate, invalid target) is a resolution
	// decision; only undecodable requests carry no event.
	started := time.Now()
	event := ResolveAuditEvent{Binding: document.Binding, TargetHost: strings.TrimSpace(document.Request.Host)}
	outcome := telemetry.ResolutionOutcomeDenied
	defer func() {
		event.Resolution = outcome
		event.Duration = time.Since(started)
		h.audit(event)
	}()

	destination, frame, targetErr := buildResolveTarget(document)
	if targetErr != nil {
		writeResolveError(w, targetErr.status, targetErr.reason)
		return
	}
	event.TargetHost = frame.Host

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
	if access.ExpectedRevision != nil && capture.Revision != *access.ExpectedRevision {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}
	event.Credential = capture.Credential

	client, err := h.clientFor(capture.Endpoint)
	if err != nil {
		log.Warnf("credential resolver client unavailable: %v", err)
		writeResolveError(w, http.StatusServiceUnavailable, "resolver unavailable")
		return
	}
	resolveCtx, cancelResolve := context.WithTimeout(r.Context(), h.timeout)
	defer cancelResolve()
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	case <-resolveCtx.Done():
		outcome = telemetry.ResolutionOutcomeTimeout
		writeResolveError(w, http.StatusGatewayTimeout, "resolver timeout")
		return
	}
	decision, err := client.Resolve(resolveCtx, ResolveInput{
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

	// Recheck (D12): discard on any identity change (subject or store
	// replaced) or captured-state change — never serve credentials resolved
	// under state the caller no longer owns.
	recheckAccess, accessErr := h.access(r)
	if accessErr != nil ||
		recheckAccess.SandboxID != access.SandboxID ||
		recheckAccess.Store != access.Store {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}
	recheck, err := recheckAccess.Store.CaptureResolve(document.Binding)
	if err != nil || recheck.Fingerprint != capture.Fingerprint ||
		access.ExpectedRevision != nil && recheck.Revision != *access.ExpectedRevision {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}

	outcome = telemetry.ResolutionOutcomeResolved
	writeResolveSuccess(w, capture.Revision, document.Binding, decision)
}

// serveGroup resolves a multi-binding request under one shared capture and one
// unified post-resolve recheck so a completed exchange binding cannot be
// committed after being deleted or replaced while a sibling resolver is still
// in flight, while unrelated mutations remain tolerated (OSEP-0025 D12).
func (h *ResolveHandler) serveGroup(w http.ResponseWriter, r *http.Request, document resolveEndpointRequest) {
	started := time.Now()
	events := make([]ResolveAuditEvent, len(document.Bindings))
	durations := make([]time.Duration, len(document.Bindings))
	rawHost := strings.TrimSpace(document.Request.Host)
	for i, name := range document.Bindings {
		events[i] = ResolveAuditEvent{
			Resolution: telemetry.ResolutionOutcomeDenied,
			Binding:    name,
			TargetHost: rawHost,
		}
	}
	defer func() {
		elapsed := time.Since(started)
		for i := range events {
			if durations[i] > 0 {
				events[i].Duration = durations[i]
			} else {
				events[i].Duration = elapsed
			}
			h.audit(events[i])
		}
	}()

	destination, frame, targetErr := buildResolveTarget(document)
	if targetErr != nil {
		writeResolveError(w, targetErr.status, targetErr.reason)
		return
	}
	for i := range events {
		events[i].TargetHost = frame.Host
	}

	access, accessErr := h.access(r)
	if accessErr != nil {
		writeResolveError(w, accessErr.Status, accessErr.Reason)
		return
	}
	for i := range events {
		events[i].Subject = access.SandboxID
	}
	if access.Store == nil {
		writeResolveError(w, http.StatusNotFound, "binding not found")
		return
	}
	group, err := access.Store.CaptureResolveGroup(document.Bindings, document.SiblingBindings)
	if err != nil {
		writeResolveError(w, http.StatusNotFound, "binding not found")
		return
	}
	if access.ExpectedRevision != nil && group.Revision != *access.ExpectedRevision {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}
	clients := make([]ResolverClient, len(group.Exchanges))
	for i, capture := range group.Exchanges {
		events[i].Credential = capture.Credential
		client, clientErr := h.clientFor(capture.Endpoint)
		if clientErr != nil {
			log.Warnf("credential resolver client unavailable: %v", clientErr)
			writeResolveError(w, http.StatusServiceUnavailable, "resolver unavailable")
			return
		}
		clients[i] = client
	}

	type groupItemResult struct {
		decision *ResolverDecision
		status   int
		reason   string
		timedOut bool
	}
	results := make([]groupItemResult, len(group.Exchanges))
	resolveCtx, cancelResolve := context.WithTimeout(r.Context(), h.timeout)
	defer cancelResolve()

	var wg sync.WaitGroup
	for i, capture := range group.Exchanges {
		wg.Add(1)
		go func(idx int, capItem ResolveCapture, bindingName string, client ResolverClient) {
			defer wg.Done()
			itemStart := time.Now()
			select {
			case h.semaphore <- struct{}{}:
				defer func() { <-h.semaphore }()
			case <-resolveCtx.Done():
				durations[idx] = time.Since(itemStart)
				results[idx] = groupItemResult{
					status:   http.StatusGatewayTimeout,
					reason:   "resolver timeout",
					timedOut: true,
				}
				return
			}
			decision, resolveErr := client.Resolve(resolveCtx, ResolveInput{
				Subject:     access.SandboxID,
				Credential:  capItem.Credential,
				Binding:     bindingName,
				Destination: destination,
				Request:     frame,
			})
			durations[idx] = time.Since(itemStart)
			if resolveErr != nil || decision == nil {
				if resolveErr == nil {
					resolveErr = resolverErrorf(ResolverUnavailable, "empty decision")
				}
				status, reason, timedOut := resolveFailureResponse(resolveErr)
				results[idx] = groupItemResult{
					status:   status,
					reason:   reason,
					timedOut: timedOut,
				}
				cancelResolve()
				return
			}
			results[idx] = groupItemResult{decision: decision}
		}(i, capture, document.Bindings[i], clients[i])
	}
	wg.Wait()

	firstFailure := -1
	for i, item := range results {
		if item.decision == nil {
			if item.timedOut {
				events[i].Resolution = telemetry.ResolutionOutcomeTimeout
			}
			if firstFailure == -1 {
				firstFailure = i
			}
		}
	}
	if firstFailure >= 0 {
		writeResolveError(w, results[firstFailure].status, results[firstFailure].reason)
		return
	}

	// Unified post-resolve recheck (D12): verify subject/store identity and every
	// captured exchange + sibling static binding fingerprint atomically before
	// returning any decision.
	recheckAccess, accessErr := h.access(r)
	if accessErr != nil ||
		recheckAccess.SandboxID != access.SandboxID ||
		recheckAccess.Store != access.Store {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}
	recheck, err := recheckAccess.Store.CaptureResolveGroup(document.Bindings, document.SiblingBindings)
	if err != nil ||
		(access.ExpectedRevision != nil && recheck.Revision != *access.ExpectedRevision) ||
		!group.matchesRecheck(recheck) {
		writeResolveError(w, http.StatusConflict, "resolution invalidated")
		return
	}

	decisions := make([]resolveEndpointResponse, len(results))
	for i, item := range results {
		events[i].Resolution = telemetry.ResolutionOutcomeResolved
		decisions[i] = buildResolveEndpointResponse(group.Revision, document.Bindings[i], item.decision)
	}
	writeResolveGroupSuccess(w, decisions)
}

// maxCachedResolverClients bounds the never-evicted per-endpoint cache; past
// the cap, clients are rebuilt per request (construction is parse-only). The
// handler semaphore keeps the process-wide concurrency bound either way.
const maxCachedResolverClients = 64

// clientFor caches one ResolverClient per endpoint; the env never changes.
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

// resolveFailureResponse maps resolver error classes to contract statuses.
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

const maxResolveGroupBindings = 64

type resolveEndpointRequest struct {
	Binding         string
	Bindings        []string
	SiblingBindings []string
	SNI             string
	Request         resolveEndpointHTTPRequest
}

type resolveEndpointHTTPRequest struct {
	Scheme string
	Host   string
	Port   int
	// PortPresent distinguishes an explicit "port" from an absent one: an
	// explicit port is validated in full (0 is not the scheme default).
	PortPresent bool
	Method      string
	Path        string
	// Headers carries the forwarded request headers, lowercased and
	// deduplicated at decode; nil (omitted) lands as an empty object.
	Headers map[string]string
}

// decodeResolveRequest strictly decodes the addon request: exact field sets,
// unique keys, no trailing data, no null scalars. Single-binding requests
// carry "binding"; grouped requests carry "bindings" and optional
// "siblingBindings".
func decodeResolveRequest(data []byte) (resolveEndpointRequest, error) {
	fields, err := strictJSONObject(data)
	if err != nil {
		return resolveEndpointRequest{}, err
	}
	for name := range fields {
		if name != "binding" && name != "bindings" && name != "siblingBindings" && name != "sni" && name != "request" {
			return resolveEndpointRequest{}, errors.New("resolve request carries an unknown field")
		}
	}
	_, hasSingle := fields["binding"]
	_, hasGroup := fields["bindings"]
	_, hasSiblings := fields["siblingBindings"]
	if hasSingle == hasGroup {
		return resolveEndpointRequest{}, errors.New("resolve request requires either binding or bindings")
	}
	document := resolveEndpointRequest{}
	if hasSingle {
		if hasSiblings || len(fields) != 3 {
			return resolveEndpointRequest{}, errors.New("resolve request requires exactly binding, sni, request")
		}
		if document.Binding, err = decodeStrictStringField(fields, "binding"); err != nil {
			return resolveEndpointRequest{}, err
		}
	} else {
		if document.Bindings, err = decodeStrictStringListField(fields, "bindings", 1, maxResolveGroupBindings); err != nil {
			return resolveEndpointRequest{}, err
		}
		if hasSiblings {
			if document.SiblingBindings, err = decodeStrictStringListField(fields, "siblingBindings", 1, maxResolveGroupBindings); err != nil {
				return resolveEndpointRequest{}, err
			}
			seen := make(map[string]struct{}, len(document.Bindings))
			for _, name := range document.Bindings {
				seen[name] = struct{}{}
			}
			for _, name := range document.SiblingBindings {
				if _, dup := seen[name]; dup {
					return resolveEndpointRequest{}, errors.New("siblingBindings cannot overlap bindings")
				}
			}
		}
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
		if name != "scheme" && name != "host" && name != "port" && name != "method" && name != "path" && name != "headers" {
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
		// JSON null into an int is a no-op success; reject like the rest.
		if bytes.Equal(trimmed, []byte("null")) {
			return resolveEndpointRequest{}, errors.New("port must be an integer")
		}
		if err := json.Unmarshal(trimmed, &document.Request.Port); err != nil {
			return resolveEndpointRequest{}, errors.New("port must be an integer")
		}
		document.Request.PortPresent = true
	}
	if headersRaw, ok := requestFields["headers"]; ok {
		if document.Request.Headers, err = decodeResolveRequestHeaders(headersRaw); err != nil {
			return resolveEndpointRequest{}, err
		}
	}
	return document, nil
}

// maxResolveRequestHeaderCount caps forwarded headers per resolve; the
// bundled addon mirrors this bound and fails closed first.
const maxResolveRequestHeaderCount = 64

// decodeResolveRequestHeaders strictly decodes the optional forwarded header
// map: non-empty string values, legal field names and values, capped count
// and value length. Case-variant keys name one header; names are normalized
// to lowercase, the resolver wire canonical form.
func decodeResolveRequestHeaders(raw json.RawMessage) (map[string]string, error) {
	fields, err := strictJSONObject(raw)
	if err != nil {
		return nil, errors.New("request headers must be an object")
	}
	if len(fields) > maxResolveRequestHeaderCount {
		return nil, errors.New("too many request headers")
	}
	headers := make(map[string]string, len(fields))
	for name := range fields {
		lowerName := strings.ToLower(name)
		if _, duplicate := headers[lowerName]; duplicate {
			return nil, errors.New("duplicate request header name")
		}
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, errors.New("invalid request header name")
		}
		value, err := decodeStrictStringField(fields, name)
		if err != nil {
			return nil, err
		}
		if value == "" {
			return nil, errors.New("request header values must be non-empty")
		}
		if len(value) > maxResolverValueBytes {
			return nil, errors.New("request header value too long")
		}
		// Values bind resolver proofs; reject illegal field-values here so
		// the denial happens before the resolver call.
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, errors.New("invalid request header value")
		}
		headers[lowerName] = value
	}
	return headers, nil
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

func decodeStrictStringListField(fields map[string]json.RawMessage, name string, minLen, maxLen int) ([]string, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, fmt.Errorf("%s field required", name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(trimmed, &rawItems); err != nil {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	if len(rawItems) < minLen || len(rawItems) > maxLen {
		return nil, fmt.Errorf("%s length out of bounds", name)
	}
	items := make([]string, 0, len(rawItems))
	seen := make(map[string]struct{}, len(rawItems))
	for _, rawItem := range rawItems {
		itemTrimmed := bytes.TrimSpace(rawItem)
		if len(itemTrimmed) == 0 || itemTrimmed[0] != '"' {
			return nil, fmt.Errorf("%s entries must be strings", name)
		}
		var value string
		if err := json.Unmarshal(itemTrimmed, &value); err != nil {
			return nil, fmt.Errorf("%s entries must be strings", name)
		}
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s entries cannot be blank", name)
		}
		if _, dup := seen[value]; dup {
			return nil, fmt.Errorf("%s cannot contain duplicate entries", name)
		}
		seen[value] = struct{}{}
		items = append(items, value)
	}
	return items, nil
}

// buildResolveTarget normalizes the outbound frame and applies the SNI gate
// (host != sni → 403): the resolver proof binds to the destination.
func buildResolveTarget(document resolveEndpointRequest) (ResolveDestination, ResolveRequestFrame, *resolveRequestError) {
	badRequest := &resolveRequestError{status: http.StatusBadRequest, reason: "malformed request"}
	forbidden := &resolveRequestError{status: http.StatusForbidden, reason: "forbidden"}

	binding := strings.TrimSpace(document.Binding)
	scheme := strings.ToLower(strings.TrimSpace(document.Request.Scheme))
	if scheme != "http" && scheme != "https" {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	port := document.Request.Port
	if !document.Request.PortPresent {
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
	if (len(document.Bindings) == 0 && binding == "") || method == "" || !strings.HasPrefix(path, "/") {
		return ResolveDestination{}, ResolveRequestFrame{}, badRequest
	}
	// Absent headers serialize as an empty object, never null.
	headers := document.Request.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	return ResolveDestination{
			Scheme:    scheme,
			Authority: host + ":" + strconv.Itoa(port),
			SNI:       sni,
		}, ResolveRequestFrame{
			Method:  method,
			Host:    host,
			Path:    path,
			Headers: headers,
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

type resolveGroupEndpointResponse struct {
	Decisions []resolveEndpointResponse `json:"decisions"`
}

func buildResolveEndpointResponse(revision int64, binding string, decision *ResolverDecision) resolveEndpointResponse {
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
	return resolveEndpointResponse{
		Revision:        revision,
		Binding:         binding,
		Headers:         headers,
		Redactions:      redactions,
		SuppressHeaders: suppress,
	}
}

func writeResolveSuccess(w http.ResponseWriter, revision int64, binding string, decision *ResolverDecision) {
	writeResolveJSON(w, http.StatusOK, buildResolveEndpointResponse(revision, binding, decision))
}

func writeResolveGroupSuccess(w http.ResponseWriter, decisions []resolveEndpointResponse) {
	encoded, err := json.Marshal(resolveGroupEndpointResponse{Decisions: decisions})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
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

// ResolveCapture is the per-binding resolve state captured under the Store
// read lock and released before the resolver call. Fingerprint digests the
// binding plus the referenced credential's source: same-name replacement and
// delete/recreate wraparound are detectable, unrelated mutations do not
// invalidate (D12). No active-snapshot tag — it flips on unrelated mutations.
type ResolveCapture struct {
	Credential  string
	Endpoint    string
	Revision    int64
	Fingerprint [sha256.Size]byte
}

// CaptureResolve captures an exchange binding's resolve inputs; static
// bindings, unknown names and missing vaults are not resolvable (ErrNotFound).
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

// ResolveGroupCapture captures all exchange and sibling static bindings matched
// by one request under a single Store read lock so post-resolve recheck verifies
// the entire selected binding set atomically.
type ResolveGroupCapture struct {
	Revision            int64
	Exchanges           []ResolveCapture
	SiblingFingerprints [][sha256.Size]byte
}

func (g ResolveGroupCapture) matchesRecheck(other ResolveGroupCapture) bool {
	if len(g.Exchanges) != len(other.Exchanges) || len(g.SiblingFingerprints) != len(other.SiblingFingerprints) {
		return false
	}
	for i := range g.Exchanges {
		if g.Exchanges[i].Fingerprint != other.Exchanges[i].Fingerprint {
			return false
		}
	}
	for i := range g.SiblingFingerprints {
		if g.SiblingFingerprints[i] != other.SiblingFingerprints[i] {
			return false
		}
	}
	return true
}

// CaptureResolveGroup captures every exchange binding and optional static
// sibling binding in one lock acquisition.
func (v *Store) CaptureResolveGroup(exchangeBindings, siblingBindings []string) (ResolveGroupCapture, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists || len(exchangeBindings) == 0 {
		return ResolveGroupCapture{}, ErrNotFound
	}
	exchanges := make([]ResolveCapture, 0, len(exchangeBindings))
	for _, name := range exchangeBindings {
		binding, ok := v.bindings[name]
		if !ok || binding.Auth.Type != "" {
			return ResolveGroupCapture{}, ErrNotFound
		}
		credential, ok := v.credentials[binding.Auth.Credential]
		if !ok || credential.SourceType != "exchange" {
			return ResolveGroupCapture{}, ErrNotFound
		}
		exchange, ok := credential.Source.(*exchangeSource)
		if !ok {
			return ResolveGroupCapture{}, ErrNotFound
		}
		exchanges = append(exchanges, ResolveCapture{
			Credential:  credential.Name,
			Endpoint:    exchange.endpoint,
			Revision:    v.revision,
			Fingerprint: resolveFingerprint(binding, credential.raw),
		})
	}
	siblings := make([][sha256.Size]byte, 0, len(siblingBindings))
	for _, name := range siblingBindings {
		binding, ok := v.bindings[name]
		if !ok || binding.Auth.Type == "" {
			return ResolveGroupCapture{}, ErrNotFound
		}
		fp, ok := staticBindingFingerprint(binding, v.credentials)
		if !ok {
			return ResolveGroupCapture{}, ErrNotFound
		}
		siblings = append(siblings, fp)
	}
	return ResolveGroupCapture{
		Revision:            v.revision,
		Exchanges:           exchanges,
		SiblingFingerprints: siblings,
	}, nil
}

func staticBindingFingerprint(binding Binding, credentials map[string]record) ([sha256.Size]byte, bool) {
	refSet := make(map[string]struct{})
	if binding.Auth.Credential != "" {
		refSet[binding.Auth.Credential] = struct{}{}
	}
	for _, header := range binding.Auth.Headers {
		if header.Credential != "" {
			refSet[header.Credential] = struct{}{}
		}
	}
	for _, sub := range binding.Auth.Substitutions {
		if sub.Credential != "" {
			refSet[sub.Credential] = struct{}{}
		}
	}
	refNames := make([]string, 0, len(refSet))
	for name := range refSet {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)

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
	for _, name := range refNames {
		rec, ok := credentials[name]
		if !ok {
			return [sha256.Size]byte{}, false
		}
		writeField("credentialName", []byte(name))
		writeField("credential", rec.raw)
	}
	return [sha256.Size]byte(h.Sum(nil)), true
}

// resolveFingerprint names the exact captured state; the recheck comparison
// happens outside any lock.
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

// ResolverConfigFromEnv derives the D13 capability gate from the operator
// env: a malformed timeout value fails its flag closed, an absent value
// keeps the default. The returned error is diagnostic only.
func ResolverConfigFromEnv() (ResolverConfig, error) {
	config := ResolverConfig{TimeoutConfigured: true}
	var diag error
	if raw := strings.TrimSpace(os.Getenv(constants.EnvCredentialResolverTimeout)); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 120 {
			config.TimeoutConfigured = false
			diag = fmt.Errorf("%s value is not an integer in [1,120]", constants.EnvCredentialResolverTimeout)
		}
	}
	return config, diag
}

// NewSubjectStore creates a per-subject store (fast-sandbox) with the D13
// gate derived from env, so reloaded exchange vaults face the same gate.
func NewSubjectStore() *Store {
	store := NewStore(nil, func() bool { return true })
	config, err := ResolverConfigFromEnv()
	if err != nil {
		log.Warnf("credential resolver operator configuration not ready: %v", err)
	}
	store.SetResolverConfig(config)
	return store
}
