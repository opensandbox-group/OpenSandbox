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

	"github.com/alibaba/opensandbox/ingress/pkg/signature"
)

type parsedRoute struct {
	sandboxID       string
	port            int
	expiresB36      string
	signature       string
	requestURI      string
	uriParsedAsOSEP bool
}

func parseHostRoute(s string) (parsedRoute, error) {
	domain := strings.Split(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), ".")
	if len(domain) < 1 {
		return parsedRoute{}, fmt.Errorf("invalid host: %s", s)
	}
	label := domain[0]

	sandboxID, port, expires, routeSig, parseErr := signature.ParseRouteToken(label)
	if parseErr == nil {
		return parsedRoute{
			sandboxID:  sandboxID,
			port:       port,
			expiresB36: expires,
			signature:  routeSig,
			requestURI: "",
		}, nil
	}

	ingressAndPort := strings.Split(label, "-")
	if len(ingressAndPort) <= 1 || ingressAndPort[0] == "" {
		return parsedRoute{}, fmt.Errorf("invalid host: %s", s)
	}
	ingress := strings.Join(ingressAndPort[:len(ingressAndPort)-1], "-")
	port, err := strconv.Atoi(ingressAndPort[len(ingressAndPort)-1])
	if err != nil {
		return parsedRoute{}, fmt.Errorf("invalid port format: %w", err)
	}
	return parsedRoute{sandboxID: ingress, port: port, expiresB36: "", signature: "", requestURI: ""}, nil
}

func parseURIRoute(path string) (parsedRoute, error) {
	if path == "" {
		return parsedRoute{}, errors.New("missing URI path")
	}

	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 5)
	if len(parts) >= 4 && parts[0] != "" {
		port, perr := signature.ParsePortSegment(parts[1])
		if perr == nil {
			if _, eerr := signature.ParseExpiresB36(parts[2]); eerr == nil {
				if signature.ValidateSignatureFormat(parts[3]) == nil {
					requestURI := "/"
					if len(parts) == 5 && parts[4] != "" {
						requestURI = "/" + parts[4]
					}
					return parsedRoute{
						sandboxID:       parts[0],
						port:            port,
						expiresB36:      parts[2],
						signature:       parts[3],
						requestURI:      requestURI,
						uriParsedAsOSEP: true,
					}, nil
				}
			}
		}
	}
	return parseURILegacy(path)
}

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
	var requestURI string
	if len(parts) >= 3 && parts[2] != "" {
		requestURI = "/" + parts[2]
	} else {
		requestURI = "/"
	}
	return parsedRoute{
		sandboxID:       sandboxID,
		port:            port,
		expiresB36:      "",
		signature:       "",
		requestURI:      requestURI,
		uriParsedAsOSEP: false,
	}, nil
}
