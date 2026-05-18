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

// Bounded event buffer (sbuf) plus at-most-one live SSE writer per command id for disconnect resume;
// GET /command/:id/resume sends buffered events then may take over as the sole live consumer.

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/sbuf"
)

var (
	resumeBuffer *sbuf.Store

	errLiveStreamPrimaryActive = errors.New("primary SSE stream is still active")
	errLiveHubNotFound         = errors.New("command live hub not found")
)

func init() {
	resumeBuffer = sbuf.NewStore(sbuf.Config{StrictMonotonic: true})
}

func deferResumeCleanup(c *CodeInterpretingController) {
	c.resumeStreamMu.Lock()
	id := c.resumeStreamID
	c.resumeStreamID = ""
	c.resumeStreamMu.Unlock()
	if id == "" {
		return
	}
	commandStreams.closeAndRemove(id)
	resumeBuffer.Delete(id)
	log.Info("command stream: hub and resume buffer cleaned up id=%s", id)
}

// --- live SSE routing (mutually exclusive main vs resume) ---

type streamRegistry struct {
	mu sync.Mutex
	m  map[string]*streamHub
}

var commandStreams = &streamRegistry{m: make(map[string]*streamHub)}

type streamHub struct {
	streamID string
	mu       sync.Mutex
	holder   *streamHolder
	done     chan struct{}
}

type streamHolder struct {
	writer http.ResponseWriter
	ctx    context.Context
}

func (r *streamRegistry) registerPrimary(id string, w http.ResponseWriter, ctx context.Context) {
	r.mu.Lock()
	h := &streamHub{
		streamID: id,
		done:     make(chan struct{}),
	}
	r.m[id] = h
	h.mu.Lock()
	h.holder = &streamHolder{writer: w, ctx: ctx}
	h.mu.Unlock()
	r.mu.Unlock()

	log.Info("command stream: primary hub registered id=%s", id)
	watchHolderRelease(h, ctx)
}

// watchHolderRelease clears h.holder when ctx is cancelled. All holder mutations use h.mu only
// (see tryAttachResume, registerPrimary, writeFrame) so r.mu and h.mu are not split across h.holder.
func watchHolderRelease(h *streamHub, ctx context.Context) {
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.holder != nil && h.holder.ctx == ctx {
			h.holder = nil
		}
	}()
}

func (r *streamRegistry) getHub(id string) *streamHub {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *streamRegistry) closeAndRemove(id string) {
	r.mu.Lock()
	h := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if h != nil {
		h.closeDone()
	}
}

func (h *streamHub) closeDone() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

func (h *streamHub) waitDone() <-chan struct{} {
	return h.done
}

func (h *streamHub) isHolderAlive() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.holder != nil && h.holder.ctx.Err() == nil
}

func (r *streamRegistry) tryAttachResume(id string, w http.ResponseWriter, ctx context.Context) (*streamHub, error) {
	r.mu.Lock()
	h := r.m[id]
	if h == nil {
		r.mu.Unlock()
		return nil, errLiveHubNotFound
	}
	h.mu.Lock()
	if h.holder != nil && h.holder.ctx.Err() == nil {
		h.mu.Unlock()
		r.mu.Unlock()
		return nil, errLiveStreamPrimaryActive
	}
	h.holder = &streamHolder{writer: w, ctx: ctx}
	h.mu.Unlock()
	r.mu.Unlock()

	watchHolderRelease(h, ctx)
	return h, nil
}

func (r *streamRegistry) writeSSE(id string, data []byte, bufEid int64, handler, summary string) {
	r.mu.Lock()
	h := r.m[id]
	r.mu.Unlock()
	if h == nil {
		if bufEid > 0 {
			_ = resumeBuffer.Append(id, bufEid, bytes.Clone(data))
		}
		return
	}
	h.writeFrame(data, bufEid, handler, summary)
}

// flushResumeTail writes all buffered events with EID > afterEid to the current holder while holding h.mu.
// Live writeFrame calls block on the same mutex, so chunks appended only to the ring during the initial
// snapshot replay cannot be missed on this connection (see ResumeCommandStream).
// Returns how many extra events were written after the initial snapshot replay.
func (h *streamHub) flushResumeTail(commandID string, afterEid int64) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holder == nil {
		return 0
	}

	tail, ok := resumeBuffer.EventsAfter(commandID, afterEid)
	if !ok || len(tail) == 0 {
		return 0
	}
	writer := h.holder.writer
	written := 0
	for _, ev := range tail {
		payload := append(append([]byte(nil), ev.Payload...), '\n', '\n')
		nw, err := writer.Write(payload)
		if err == nil && nw != len(payload) {
			err = io.ErrShortWrite
		}
		if err != nil {
			log.Error("flushResumeTail: write eid=%d: %v", ev.EID, err)
			return written
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		written++
	}
	return written
}

func (h *streamHub) writeFrame(data []byte, bufEid int64, handler, summary string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	payload := append(data, '\n', '\n')
	if h.holder != nil {
		n, err := h.holder.writer.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		if err != nil {
			log.Error("StreamEvent.%s write data %s error: %v", handler, summary, err)
		} else if flusher, ok := h.holder.writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	if bufEid > 0 {
		_ = resumeBuffer.Append(h.streamID, bufEid, bytes.Clone(data))
	}
}
