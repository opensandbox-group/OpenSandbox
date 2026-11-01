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
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// exchangeSource holds no local material: header sets resolve per request
// from the endpoint's resolver. Resolve fails closed — exchange credentials
// surface only as snapshot markers, never rendered values.
type exchangeSource struct {
	endpoint string
}

func (s *exchangeSource) Type() string { return "exchange" }

func (s *exchangeSource) Resolve(_ context.Context) (string, error) {
	return "", fmt.Errorf("exchange credential cannot be resolved inline; it resolves per request via the credential resolver")
}

// exchangeSourceFactory creates an exchangeSource from the raw JSON "source"
// object. Expected shape: {"type": "exchange", "endpoint": "<uri>"}.
// Unknown fields are rejected to match the OpenAPI additionalProperties: false
// contract.
func exchangeSourceFactory(raw json.RawMessage) (CredentialSource, error) {
	var src struct {
		Type     string `json:"type"`
		Endpoint string `json:"endpoint"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("parse exchange credential source: %w", err)
	}
	endpoint, err := validateResolverEndpoint(src.Endpoint)
	if err != nil {
		return nil, err
	}
	return &exchangeSource{endpoint: endpoint}, nil
}

func validateResolverEndpoint(endpoint string) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		path := strings.TrimPrefix(endpoint, "unix://")
		if !strings.HasPrefix(path, "/") || len(path) < 2 {
			return "", fmt.Errorf("exchange credential endpoint %q must be an absolute unix:///path", endpoint)
		}
		return endpoint, nil
	case strings.HasPrefix(endpoint, "https://"):
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return "", fmt.Errorf("exchange credential endpoint %q is not a valid https URL", endpoint)
		}
		if parsed.Hostname() == "" || parsed.User != nil {
			return "", fmt.Errorf("exchange credential endpoint %q must address https://host[:port]", endpoint)
		}
		if port := parsed.Port(); port != "" {
			value, err := strconv.Atoi(port)
			if err != nil || value < 1 || value > 65535 {
				return "", fmt.Errorf("exchange credential endpoint %q has an invalid port", endpoint)
			}
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
			return "", fmt.Errorf("exchange credential endpoint %q must not carry a query or fragment", endpoint)
		}
		return endpoint, nil
	default:
		return "", fmt.Errorf("exchange credential endpoint %q must use the unix:/// or https:// scheme", endpoint)
	}
}
