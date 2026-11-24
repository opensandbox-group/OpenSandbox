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
	"mime"
	"net/http"
	"strings"
)

// isGRPCRequest reports whether r is native gRPC: HTTP/2 with an
// application/grpc or application/grpc+<codec> content type. gRPC-Web is
// excluded because it is HTTP/1.1-compatible and its backends may not speak
// HTTP/2.
func isGRPCRequest(r *http.Request) bool {
	if r.ProtoMajor != 2 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "application/grpc" || strings.HasPrefix(mediaType, "application/grpc+")
}
