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
	"strings"
)

func rewriteRequestURL(r *http.Request, target *url.URL) {
	r.URL.Scheme = target.Scheme
	r.URL.Host = target.Host
	r.URL.Path = target.Path
	r.URL.RawPath = target.RawPath
	r.URL.RawQuery = target.RawQuery
	r.Host = target.Host
}

func (p *Proxy) resolveRealHost(host *sandboxHost, requestURL *url.URL) (*url.URL, int, error) {
	if host.info == nil {
		return nil, http.StatusBadGateway, errors.New("provider returned no endpoint information")
	}
	requestPath, requestRawPath := effectiveRequestPaths(host.requestURI, host.requestRawPath, requestURL)
	if host.info.UpstreamURL == "" {
		if host.endpoint == "" {
			return nil, http.StatusBadGateway, errors.New("provider returned an empty endpoint")
		}
		return &url.URL{Host: fmt.Sprintf("%s:%d", host.endpoint, host.port), Path: requestPath, RawPath: nonCanonicalRawPath(requestPath, requestRawPath), RawQuery: requestURL.RawQuery}, 0, nil
	}
	upstream, err := url.Parse(host.info.UpstreamURL)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	upstreamRawPath := upstream.EscapedPath()
	upstream.Path = joinURLPath(upstream.Path, requestPath)
	upstream.RawPath = nonCanonicalRawPath(upstream.Path, joinURLPath(upstreamRawPath, requestRawPath))
	if upstream.RawQuery == "" {
		upstream.RawQuery = requestURL.RawQuery
	} else if requestURL.RawQuery != "" {
		upstream.RawQuery += "&" + requestURL.RawQuery
	}
	return upstream, 0, nil
}

func effectiveRequestPaths(path, rawPath string, requestURL *url.URL) (string, string) {
	if path != "" {
		return path, rawPath
	}
	if requestURL == nil {
		return "", ""
	}
	return requestURL.Path, requestURL.EscapedPath()
}

func nonCanonicalRawPath(path, escapedPath string) string {
	if escapedPath == "" || escapedPath == (&url.URL{Path: path}).EscapedPath() {
		return ""
	}
	return escapedPath
}

func joinURLPath(base, suffix string) string {
	if base == "" {
		base = "/"
	}
	if suffix == "" || suffix == "/" {
		return strings.TrimRight(base, "/") + "/"
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}
