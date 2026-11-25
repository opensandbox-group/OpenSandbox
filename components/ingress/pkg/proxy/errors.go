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
	"net/http"
	"net/url"
	"strconv"

	"google.golang.org/grpc/codes"

	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
)

func ingressRouteErrHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, routescope.ErrInvalidScope) || errors.Is(err, routescope.ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	return signature.HTTPStatusForIngressErr(err)
}

func providerErrHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return http.StatusNotFound
	}

	if errors.Is(err, sandbox.ErrSandboxNotReady) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, sandbox.ErrTargetUnsupported) {
		return http.StatusNotImplemented
	}

	return http.StatusBadGateway
}

// grpcCodeForHTTPStatus maps an ingress-generated HTTP status to the gRPC code
// a gRPC caller should see.
func grpcCodeForHTTPStatus(status int) codes.Code {
	switch status {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return codes.Unavailable
	default:
		return codes.Unknown
	}
}

// writeIngressError answers a request the ingress itself rejects. Non-gRPC
// requests get http.Error. gRPC requests get a trailers-only response (HTTP
// 200 with grpc-status in the headers), because gRPC clients ignore
// grpc-status on any other HTTP status; the HTTP status is still recorded for
// metrics.
func writeIngressError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if !isGRPCRequest(r) {
		http.Error(w, msg, status)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/grpc")
	header.Set("Grpc-Status", strconv.Itoa(int(grpcCodeForHTTPStatus(status))))
	header.Set("Grpc-Message", url.PathEscape(msg))
	if recorder, ok := w.(interface{ commitStatus(int) }); ok {
		recorder.commitStatus(status)
	}
	w.WriteHeader(http.StatusOK)
}
