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

package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlushResumeTail_NilHubNoPanic(t *testing.T) {
	var h *streamHub
	h.flushResumeTail("any", 0)
}

func TestFlushResumeTail_WritesBufferedEvents(t *testing.T) {
	cmdID := "flush-resume-test-cmd"
	payload := []byte(`{"type":"stdout","eid":1}`)
	require.NoError(t, resumeBuffer.Append(cmdID, 1, payload))

	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &streamHub{
		streamID: cmdID,
		done:     make(chan struct{}),
		holder:   &streamHolder{writer: w, ctx: ctx},
	}
	h.flushResumeTail(cmdID, 0)

	body := w.Body.String()
	require.Contains(t, body, "stdout")
	require.Contains(t, body, `"eid":1`)
}
