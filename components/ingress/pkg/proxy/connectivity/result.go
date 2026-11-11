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

package connectivity

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// Result classifies the outcome of one TCP connection attempt. It deliberately
// excludes HTTP, TLS, and application protocol outcomes.
type Result string

const (
	ResultSuccess     Result = "success"
	ResultTimeout     Result = "timeout"
	ResultUnreachable Result = "unreachable"
	ResultRefused     Result = "refused"
	ResultDNS         Result = "dns_error"
	ResultCanceled    Result = "canceled"
	ResultOther       Result = "other"
)

// ClassifyConnectError classifies an error returned by a TCP DialContext call.
func ClassifyConnectError(err error) Result {
	if err == nil {
		return ResultSuccess
	}

	switch {
	case errors.Is(err, syscall.ETIMEDOUT):
		return ResultTimeout
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.EHOSTDOWN):
		return ResultUnreachable
	case errors.Is(err, syscall.ECONNREFUSED):
		return ResultRefused
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ResultDNS
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ResultTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ResultCanceled
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ResultTimeout
	}

	return ResultOther
}

func isDegradationSignal(result Result) bool {
	return result == ResultTimeout || result == ResultUnreachable
}
