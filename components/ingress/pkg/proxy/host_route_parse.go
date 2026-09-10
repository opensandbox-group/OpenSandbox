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

package proxy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
)

// parsedRoute is one decoded ingress route before provider resolution.
type parsedRoute struct {
	routeKind  sandbox.RouteKind
	namespace  string
	sandboxID  string
	port       int
	expiresB36 string
	signature  string

	// requestURI is the path forwarded upstream after route metadata is stripped.
	requestURI     string
	requestRawPath string

	// signedRoute reports that the route came from the signed four-segment URI
	// format, so four leading segments must be stripped from the request path.
	signedRoute bool
}

// parseHostRoute parses a header-mode route from a host value
// "<route-label>.<domain...>". The first DNS label is either a signed token
// "<sandbox-id>-<port>-<expires_b36>-<signature>" or a legacy
// "<sandbox-id>-<port>" pair. Scheme prefixes are tolerated.
func parseHostRoute(s string) (parsedRoute, error) {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	label := strings.Split(trimmed, ".")[0]

	if sandboxID, port, expires, routeSig, err := signature.ParseRouteToken(label); err == nil {
		return parsedRoute{
			sandboxID:  sandboxID,
			port:       port,
			expiresB36: expires,
			signature:  routeSig,
		}, nil
	}

	// Fallback for legacy labels whose sandbox ID itself contains hyphens:
	// everything before the last "-<port>" segment is the sandbox ID.
	ingressAndPort := strings.Split(label, "-")
	if len(ingressAndPort) <= 1 || ingressAndPort[0] == "" {
		return parsedRoute{}, fmt.Errorf("invalid host: %s", s)
	}
	sandboxID := strings.Join(ingressAndPort[:len(ingressAndPort)-1], "-")
	port, err := strconv.Atoi(ingressAndPort[len(ingressAndPort)-1])
	if err != nil {
		return parsedRoute{}, fmt.Errorf("invalid port format: %w", err)
	}
	return parsedRoute{sandboxID: sandboxID, port: port}, nil
}

// parseURIRoute parses a URI-mode route. Signed format:
// /<sandbox-id>/<port>/<expires_b36>/<signature>/<request-path>.
// Legacy format: /<sandbox-id>/<port>/<request-path>.
func parseURIRoute(path string) (parsedRoute, error) {
	if path == "" {
		return parsedRoute{}, errors.New("missing URI path")
	}

	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 5)
	if route, ok := parseSignedURIRoute(parts); ok {
		return route, nil
	}
	return parseURILegacy(path)
}

// parseSignedURIRoute decodes the signed four-segment URI format when parts
// match it; otherwise it reports false and the caller falls back to the legacy
// two-segment format.
func parseSignedURIRoute(parts []string) (parsedRoute, bool) {
	if len(parts) < 4 || parts[0] == "" {
		return parsedRoute{}, false
	}
	port, err := signature.ParsePortSegment(parts[1])
	if err != nil {
		return parsedRoute{}, false
	}
	expiresB36 := parts[2]
	if _, err := signature.ParseExpiresB36(expiresB36); err != nil {
		return parsedRoute{}, false
	}
	routeSig := parts[3]
	if err := signature.ValidateSignatureFormat(routeSig); err != nil {
		return parsedRoute{}, false
	}
	requestURI := "/"
	if len(parts) == 5 && parts[4] != "" {
		requestURI = "/" + parts[4]
	}
	return parsedRoute{
		sandboxID:   parts[0],
		port:        port,
		expiresB36:  expiresB36,
		signature:   routeSig,
		requestURI:  requestURI,
		signedRoute: true,
	}, true
}

// parseURILegacy parses the two-segment URI format
// /<sandbox-id>/<port>/<request-path>.
func parseURILegacy(path string) (parsedRoute, error) {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 {
		return parsedRoute{}, fmt.Errorf("invalid URI path format: expected '/<sandbox-id>/<sandbox-port>/<path-to-request>', got: %s", path)
	}
	sandboxID := parts[0]
	if sandboxID == "" {
		return parsedRoute{}, errors.New("missing sandbox-id or sandbox-port in URI path")
	}
	port, err := signature.ParsePortSegment(parts[1])
	if err != nil {
		return parsedRoute{}, fmt.Errorf("invalid port format: %w", err)
	}
	requestURI := "/"
	if len(parts) >= 3 && parts[2] != "" {
		requestURI = "/" + parts[2]
	}
	return parsedRoute{
		sandboxID:  sandboxID,
		port:       port,
		requestURI: requestURI,
	}, nil
}
