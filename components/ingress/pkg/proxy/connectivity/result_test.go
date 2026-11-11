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
	"testing"
)

func TestClassifyConnectError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Result
	}{
		{name: "success", want: ResultSuccess},
		{name: "timeout", err: syscall.ETIMEDOUT, want: ResultTimeout},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: ResultUnreachable},
		{name: "network unreachable", err: syscall.ENETUNREACH, want: ResultUnreachable},
		{name: "network down", err: syscall.ENETDOWN, want: ResultUnreachable},
		{name: "host down", err: syscall.EHOSTDOWN, want: ResultUnreachable},
		{name: "refused", err: syscall.ECONNREFUSED, want: ResultRefused},
		{name: "DNS", err: &net.DNSError{Err: "no such host"}, want: ResultDNS},
		{name: "deadline", err: context.DeadlineExceeded, want: ResultTimeout},
		{name: "canceled", err: context.Canceled, want: ResultCanceled},
		{name: "other", err: errors.New("broken connection"), want: ResultOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyConnectError(test.err); got != test.want {
				t.Fatalf("ClassifyConnectError(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
