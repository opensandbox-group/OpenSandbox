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

package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

const execdInternalInitPath = "/internal/init"

var errForbiddenExecdInternalPath = errors.New("/internal/init on port 44772 is not accessible through ingress")

func (p *Proxy) getSandboxHostDefinition(r *http.Request) (*sandboxHost, int, error) {
	start := time.Now()
	host, status, err := p.doGetSandboxHostDefinition(r)
	durationMs := float64(time.Since(start)) / float64(time.Millisecond)

	result := "success"
	if err != nil {
		result = routingResultFromErr(err)
	}
	telemetry.RecordRouting(result, durationMs)
	return host, status, err
}

func routingResultFromErr(err error) string {
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return "not_found"
	}
	if errors.Is(err, sandbox.ErrSandboxNotReady) {
		return "not_ready"
	}
	return "error"
}

func (p *Proxy) doGetSandboxHostDefinition(r *http.Request) (*sandboxHost, int, error) {
	pr, status, err := p.parseRequestedRoute(r)
	if status != 0 {
		if err == nil {
			err = errors.New("invalid ingress route")
		}
		return nil, status, err
	}

	if err != nil || pr.sandboxID == "" || pr.port == 0 {
		if err == nil {
			err = errors.New("missing sandbox ID or port")
		}
		return nil, ingressRouteErrHTTPStatus(err), fmt.Errorf("invalid ingress route: %w", err)
	}
	if status, err := p.validateParsedRoute(pr, r); err != nil {
		return nil, status, err
	}

	target := sandbox.EndpointTarget{
		RouteKind: pr.routeKind,
		Namespace: pr.namespace,
		SandboxID: pr.sandboxID,
		Port:      pr.port,
	}
	endpoint, err := p.sandboxProvider.ResolveEndpoint(r.Context(), target)
	if err != nil {
		// Resume invalidates cached routes, so resolve again after a successful wake.
		if p.waker != nil && pr.routeKind == sandbox.RouteKindFastSandbox && errors.Is(err, sandbox.ErrSandboxNotReady) {
			if wakeErr := p.waker.Wake(r.Context(), target); wakeErr == nil {
				endpoint, err = p.sandboxProvider.ResolveEndpoint(r.Context(), target)
			} else {
				return nil, httpStatusForWakeErr(wakeErr), &wakeGateError{target: target, err: fmt.Errorf("wake sandbox: %w", wakeErr)}
			}
		}
		if err != nil {
			return nil, providerErrHTTPStatus(err), err
		}
	}

	need := endpoint.AccessVerificationRequired()

	// Unsigned sandboxes must retain path segments that only resemble a signed route.
	if p.mode == ModeURI && !need && pr.signedRoute {
		pr, err = parseURILegacy(r.URL.Path)
		if err != nil {
			return nil, ingressRouteErrHTTPStatus(err), err
		}
		pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 2)
		if err := checkBlockedExecdInternalPath(pr, r.URL); err != nil {
			return nil, http.StatusForbidden, err
		}
	}

	present, accessTok := signature.SecureAccessHeaderInfo(r)
	if err := signature.CheckIngressSecureAccess(signature.IngressAccessInput{
		Secure:                    need,
		ExpectedAccessToken:       endpoint.SecureAccessToken,
		SecureAccessHeaderPresent: present,
		RequestedAccessToken:      accessTok,
		ExpiresB36:                pr.expiresB36,
		Signature:                 pr.signature,
		SandboxID:                 pr.sandboxID,
		Port:                      pr.port,
		Verifier:                  p.secure,
	}); err != nil {
		return nil, ingressRouteErrHTTPStatus(err), err
	}

	return &sandboxHost{
		routeKind:      pr.routeKind,
		namespace:      pr.namespace,
		ingressKey:     pr.sandboxID,
		port:           pr.port,
		endpoint:       endpoint.Endpoint,
		info:           endpoint,
		requestURI:     pr.requestURI,
		requestRawPath: pr.requestRawPath,
	}, 0, nil
}

func (p *Proxy) parseRequestedRoute(r *http.Request) (parsedRoute, int, error) {
	switch p.mode {
	case ModeHeader:
		return p.parseRequestedHeaderRoute(r)
	case ModeURI:
		return p.parseRequestedURIRoute(r)
	default:
		return parsedRoute{}, http.StatusBadRequest, fmt.Errorf("unknown ingress mode: %s", p.mode)
	}
}

func (p *Proxy) parseRequestedHeaderRoute(r *http.Request) (parsedRoute, int, error) {
	targetHost := p.parseTargetHostByHeader(r)
	if targetHost == "" {
		return parsedRoute{}, http.StatusBadRequest, fmt.Errorf("missing header '%s' or 'Host'", SandboxIngress)
	}
	if routescope.IsToken(targetHost) {
		pr, err := p.parseFastSandboxScope(targetHost, "")
		return pr, 0, err
	}
	if r.Header.Get(SandboxIngress) == "" && r.Header.Get(DeprecatedSandboxIngress) == "" {
		targetHost = strings.ToLower(targetHost)
	}
	pr, err := parseHostRoute(targetHost)
	if err != nil {
		return pr, 0, err
	}
	// Fast Sandbox IDs are lowercase Kubernetes names, including labels copied from Host.
	if fastSandboxID := strings.ToLower(pr.sandboxID); strings.HasPrefix(fastSandboxID, "fsb-") {
		pr.sandboxID = fastSandboxID
		// Unsigned labels use the fixed namespace; signed labels must use scope verification.
		if p.fastSandboxNamespace == "" || pr.signature != "" {
			return parsedRoute{}, 0, fmt.Errorf("%w: unsigned Fast Sandbox routes require --fast-sandbox-namespace and an unsigned route label", routescope.ErrUnauthorized)
		}
		pr.routeKind = sandbox.RouteKindFastSandbox
		pr.namespace = p.fastSandboxNamespace
	}
	return pr, 0, nil
}

func (p *Proxy) parseRequestedURIRoute(r *http.Request) (parsedRoute, int, error) {
	if r.URL == nil || r.URL.Path == "" {
		return parsedRoute{}, http.StatusBadRequest, errors.New("missing URI path")
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	first, rest, found := strings.Cut(trimmed, "/")
	if routescope.IsToken(first) {
		requestURI := "/"
		if found && rest != "" {
			requestURI += rest
		}
		pr, err := p.parseFastSandboxScope(first, requestURI)
		if err == nil {
			pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 1)
		}
		return pr, 0, err
	}

	pr, err := parseURIRoute(r.URL.Path)
	if err != nil {
		return pr, 0, err
	}
	// Signed routes carry four leading route segments; legacy routes two.
	if pr.signedRoute {
		pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 4)
	} else {
		pr.requestRawPath = escapedPathSuffix(r.URL.EscapedPath(), 2)
	}
	return pr, 0, nil
}

func escapedPathSuffix(path string, prefixSegments int) string {
	trimmed := strings.TrimPrefix(path, "/")
	for range prefixSegments {
		_, rest, found := strings.Cut(trimmed, "/")
		if !found {
			return "/"
		}
		trimmed = rest
	}
	if trimmed == "" {
		return "/"
	}
	return "/" + trimmed
}

func (p *Proxy) parseFastSandboxScope(token, requestURI string) (parsedRoute, error) {
	if p.scope == nil {
		return parsedRoute{}, fmt.Errorf("%w: verifier is not configured", routescope.ErrUnauthorized)
	}
	scope, err := p.scope.Verify(token)
	if err != nil {
		return parsedRoute{}, err
	}
	return parsedRoute{
		routeKind:  sandbox.RouteKindFastSandbox,
		namespace:  scope.Namespace,
		sandboxID:  scope.SandboxID,
		port:       scope.Port,
		requestURI: requestURI,
	}, nil
}

func (p *Proxy) parseTargetHostByHeader(r *http.Request) string {
	targetHost := r.Header.Get(SandboxIngress)
	if targetHost != "" {
		return targetHost
	}
	deprecatedTargetHost := r.Header.Get(DeprecatedSandboxIngress)
	if deprecatedTargetHost != "" {
		return deprecatedTargetHost
	}

	return r.Host
}

type sandboxHost struct {
	routeKind      sandbox.RouteKind
	namespace      string
	ingressKey     string
	port           int
	endpoint       string
	info           *sandbox.EndpointInfo
	requestURI     string
	requestRawPath string
}

func (p *Proxy) validateParsedRoute(pr parsedRoute, r *http.Request) (int, error) {
	if pr.routeKind == sandbox.RouteKindLegacy && strings.HasPrefix(pr.sandboxID, "fsb-") {
		err := fmt.Errorf("%w: Fast Sandbox routes require a signed scope or configured wildcard Host routing", routescope.ErrUnauthorized)
		return ingressRouteErrHTTPStatus(err), err
	}
	if pr.routeKind == sandbox.RouteKindFastSandbox && p.fastSandboxNamespace != "" && pr.namespace != p.fastSandboxNamespace {
		err := fmt.Errorf("%w: Fast Sandbox namespace is not allowed", routescope.ErrUnauthorized)
		return ingressRouteErrHTTPStatus(err), err
	}
	if _, required := p.sandboxProvider.(sandbox.AuthenticatedRouteProvider); required && pr.namespace == "" {
		err := fmt.Errorf("%w: Fast Sandbox provider requires an authenticated route scope", routescope.ErrUnauthorized)
		return ingressRouteErrHTTPStatus(err), err
	}
	// Reject control-plane-only paths before resolution can wake sandboxes or record activity.
	if err := checkBlockedExecdInternalPath(pr, r.URL); err != nil {
		return http.StatusForbidden, err
	}
	return 0, nil
}

func checkBlockedExecdInternalPath(pr parsedRoute, requestURL *url.URL) error {
	reqPath, reqRawPath := effectiveRequestPaths(pr.requestURI, pr.requestRawPath, requestURL)
	if isBlockedExecdInternalPath(pr.port, reqPath, reqRawPath) {
		return errForbiddenExecdInternalPath
	}
	return nil
}

func isBlockedExecdInternalPath(port int, requestPath, rawPath string) bool {
	if port != sandbox.ExecdPort {
		return false
	}
	return matchesExecdInternalInitPath(requestPath) || matchesExecdInternalInitPath(rawPath)
}

func matchesExecdInternalInitPath(candidate string) bool {
	if candidate == "" {
		return false
	}
	if matchesCleanedExecdInternalInitPath(candidate) {
		return true
	}
	if unescaped, err := url.PathUnescape(candidate); err == nil && unescaped != candidate {
		return matchesCleanedExecdInternalInitPath(unescaped)
	}
	return false
}

func matchesCleanedExecdInternalInitPath(raw string) bool {
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	cleaned := strings.ToLower(filepath.ToSlash(filepath.Clean(raw)))
	return cleaned == execdInternalInitPath || strings.HasPrefix(cleaned, execdInternalInitPath+"/")
}
