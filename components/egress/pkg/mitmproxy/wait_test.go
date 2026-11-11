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
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitListenPortContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	require.NoError(t, WaitListenPortContext(context.Background(), listener.Addr().String(), time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = WaitListenPortContext(ctx, "127.0.0.1:1", time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), 200*time.Millisecond)
}
