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
	"net"
	"strings"
	"time"
)

// DialContextFunc matches net.Dialer.DialContext.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// WrapDialContext observes exactly one result for every TCP invocation of next.
func WrapDialContext(next DialContextFunc, observer Observer, protocol string) DialContextFunc {
	if next == nil {
		return nil
	}
	return wrapDialContext(next, observer, protocol, time.Now)
}

func wrapDialContext(next DialContextFunc, observer Observer, protocol string, now func() time.Time) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		startedAt := now()
		conn, err := next(ctx, network, address)
		finishedAt := now()
		if observer == nil || !strings.HasPrefix(network, "tcp") {
			return conn, err
		}

		result := ClassifyConnectError(err)
		// A request can be canceled while DialContext is returning a more
		// specific network error. Preserve that error and use cancellation only
		// when the dial result itself cannot be classified.
		if err != nil && result == ResultOther && ctx.Err() == context.Canceled {
			result = ResultCanceled
		}
		observer.Observe(Observation{
			At:       finishedAt,
			Protocol: protocol,
			Target:   address,
			Result:   result,
			Duration: finishedAt.Sub(startedAt),
		})
		return conn, err
	}
}
