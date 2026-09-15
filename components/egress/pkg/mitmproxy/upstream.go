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

package mitmproxy

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// UpstreamProxySpec is the validated chained upstream proxy endpoint parsed
// from OPENSANDBOX_EGRESS_UPSTREAM_PROXY.
type UpstreamProxySpec struct {
	Scheme string // "http" or "https"
	Host   string
	Port   int
}

// upstreamProxyScriptPath is the bundled upstream-proxy addon shipped via the
// egress Dockerfile (COPY components/egress/mitmscripts /var/egress/mitmscripts).
// Loaded after system.py and before user addons when chaining is enabled.
const upstreamProxyScriptPath = "/var/egress/mitmscripts/upstream_proxy.py"

// UpstreamProxyFromEnv parses OPENSANDBOX_EGRESS_UPSTREAM_PROXY. It returns
// (nil, nil) when the env is unset and an error when the configuration is
// inconsistent (bad URL, or _AUTH without _PROXY).
func UpstreamProxyFromEnv() (*UpstreamProxySpec, error) {
	raw := strings.TrimSpace(os.Getenv(constants.EnvUpstreamProxy))
	if raw == "" {
		if strings.TrimSpace(os.Getenv(constants.EnvUpstreamProxyAuth)) != "" {
			return nil, fmt.Errorf("%s is set but %s is empty", constants.EnvUpstreamProxyAuth, constants.EnvUpstreamProxy)
		}
		return nil, nil
	}
	spec, err := parseUpstreamProxy(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", constants.EnvUpstreamProxy, err)
	}
	return &spec, nil
}

// validateUpstreamProxyEnv fails fast on inconsistent chained-proxy env
// configuration, before mitmdump is spawned: the addon cannot fix a bad spec
// at runtime, and a silent fallback to direct egress would be a policy hole.
func validateUpstreamProxyEnv() error {
	_, err := UpstreamProxyFromEnv()
	return err
}

// parseUpstreamProxy parses "scheme://host[:port]" into a spec. The port
// defaults to the URL scheme default (80 for http, 443 for https). Userinfo,
// query and fragment are rejected so the value can only ever carry an address;
// credentials belong exclusively in OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH.
func parseUpstreamProxy(raw string) (UpstreamProxySpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return UpstreamProxySpec{}, fmt.Errorf("value is empty")
	}
	if !strings.Contains(raw, "://") {
		return UpstreamProxySpec{}, fmt.Errorf("missing scheme, want http://host:port or https://host:port")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return UpstreamProxySpec{}, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return UpstreamProxySpec{}, fmt.Errorf("unsupported scheme %q, want http or https", u.Scheme)
	}
	if u.User != nil {
		return UpstreamProxySpec{}, fmt.Errorf("userinfo is not allowed, use %s for credentials", constants.EnvUpstreamProxyAuth)
	}
	host := u.Hostname()
	if host == "" {
		return UpstreamProxySpec{}, fmt.Errorf("missing host")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return UpstreamProxySpec{}, fmt.Errorf("query and fragment are not allowed")
	}
	port := 0
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return UpstreamProxySpec{}, fmt.Errorf("invalid port %q", p)
		}
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	if strings.ContainsAny(host, " \t\r\n/@") {
		return UpstreamProxySpec{}, fmt.Errorf("invalid host %q", host)
	}
	return UpstreamProxySpec{Scheme: u.Scheme, Host: strings.ToLower(host), Port: port}, nil
}
