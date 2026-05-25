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
	"net/http"

	"github.com/alibaba/opensandbox/ingress/pkg/signature"
)

type Mode string

const (
	ModeHeader Mode = "header"
	ModeURI    Mode = "uri"
)

func (p *Proxy) getSandboxHostDefinition(r *http.Request) (*sandboxHost, int, error) {
	var pr parsedRoute
	var err error

	switch p.mode {
	case ModeHeader:
		targetHost := p.parseTargetHostByHeader(r)
		if targetHost == "" {
			return nil, http.StatusBadRequest, fmt.Errorf("missing header '%s' or 'Host'", SandboxIngress)
		}
		pr, err = parseHostRoute(targetHost)
	case ModeURI:
		if r.URL == nil || r.URL.Path == "" {
			return nil, http.StatusBadRequest, errors.New("missing URI path")
		}
		pr, err = parseURIRoute(r.URL.Path)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("unknown ingress mode: %s", p.mode)
	}

	if err != nil || pr.sandboxID == "" || pr.port == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid ingress route: %w", err)
	}

	endpoint, err := p.sandboxProvider.GetEndpoint(pr.sandboxID)
	if err != nil {
		return nil, providerErrHTTPStatus(err), err
	}

	need := endpoint.AccessVerificationRequired()

	if p.mode == ModeURI && !need && pr.uriParsedAsOSEP {
		pr, err = parseURILegacy(r.URL.Path)
		if err != nil {
			return nil, ingressRouteErrHTTPStatus(err), err
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
		ingressKey: pr.sandboxID,
		port:       pr.port,
		endpoint:   endpoint.Endpoint,
		requestURI: pr.requestURI,
	}, 0, nil
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
	ingressKey string
	port       int
	endpoint   string
	requestURI string
}
