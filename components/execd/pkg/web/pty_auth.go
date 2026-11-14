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

package web

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// ptyWebSocketAccessToken extracts and strips the credential subprotocol before
// the PTY handler negotiates the application subprotocol.
func ptyWebSocketAccessToken(r *http.Request) (string, bool) {
	const header = "Sec-WebSocket-Protocol"
	var protocols []string
	var encoded string
	credentialCount := 0
	hasPTYProtocol := false
	for _, value := range r.Header.Values(header) {
		for _, part := range strings.Split(value, ",") {
			protocol := strings.TrimSpace(part)
			if strings.HasPrefix(protocol, model.PTYWebSocketTokenPrefix) {
				credentialCount++
				encoded = strings.TrimPrefix(protocol, model.PTYWebSocketTokenPrefix)
				continue
			}
			if protocol == model.PTYWebSocketProtocol {
				hasPTYProtocol = true
			}
			if protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
	}
	r.Header.Del(header)
	if len(protocols) > 0 {
		r.Header.Set(header, strings.Join(protocols, ", "))
	}
	if credentialCount == 0 {
		return "", true
	}
	if credentialCount != 1 || !hasPTYProtocol {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) == 0 || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return "", false
	}
	return string(decoded), true
}
