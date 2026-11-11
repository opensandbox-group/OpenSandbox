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

package mitmproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// WaitListenPort polls until addr accepts TCP or d elapses.
func WaitListenPort(addr string, d time.Duration) error {
	return WaitListenPortContext(context.Background(), addr, d)
}

// WaitListenPortContext polls until addr accepts TCP, d elapses, or ctx is
// cancelled. Cancellation lets process supervisors fence a half-launched child
// before shutdown returns.
func WaitListenPortContext(ctx context.Context, addr string, d time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		c, err := (&net.Dialer{Timeout: 150 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("timeout waiting for %s", addr)
			}
			return err
		}
		timer := time.NewTimer(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timeout waiting for %s", addr)
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
