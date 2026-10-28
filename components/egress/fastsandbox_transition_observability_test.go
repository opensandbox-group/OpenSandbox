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

package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/stretchr/testify/require"
)

type rebindEventLogger struct {
	logger.Logger
	mu     sync.Mutex
	events []string
}

func (l *rebindEventLogger) record(level, format string, args ...any) {
	if format != "subject_rebind_pending" && format != "subject_rebind_cleared" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, level+":"+fmt.Sprintf(format, args...))
}
func (l *rebindEventLogger) Infof(format string, args ...any) { l.record("info", format, args...) }
func (l *rebindEventLogger) Warnf(format string, args ...any) { l.record("warn", format, args...) }
func (l *rebindEventLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func TestTransitionPendingEventsAreBoundedAndExact(t *testing.T) {
	for _, completion := range []string{"retry", "remove", "takeover"} {
		t.Run(completion, func(t *testing.T) {
			sink := &rebindEventLogger{Logger: log.Logger}
			previous := log.Logger
			log.Logger = sink
			defer func() { log.Logger = previous }()
			srv, reg, nft := fastSandboxTestServer(t)
			subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
			body := bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)
			nft.removeErr = errors.New("sensitive injected detail")
			require.Equal(t, http.StatusInternalServerError, doAction(t, srv, body).Code)
			require.Equal(t, []string{"warn:subject_rebind_pending"}, sink.snapshot())
			// Polling reads and stale removal must not produce repeated pending events.
			require.Equal(t, http.StatusServiceUnavailable, doVault(t, srv, http.MethodGet, testUID, "", testFenceR).Code)
			require.Equal(t, http.StatusServiceUnavailable, doAction(t, srv, hookBody(t, testUID, "B", "B-att", constants.HookDataPlaneReady)).Code)
			require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, testFenceR, testFenceA)).Code)
			require.Len(t, sink.snapshot(), 1)
			nft.removeErr = nil
			switch completion {
			case "retry":
				require.Equal(t, http.StatusOK, doAction(t, srv, body).Code)
			case "remove":
				require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, "B", "B-att")).Code)
			case "takeover":
				require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "successor", testIP, "C", "C-att", 1, nil)).Code)
			}
			require.Nil(t, transitionCurrentToken(srv, subj))
			require.Equal(t, []string{"warn:subject_rebind_pending", "info:subject_rebind_cleared"}, sink.snapshot())
			srv.finishRebind(subj, nil)
			srv.dropSubjectState(subj)
			require.Len(t, sink.snapshot(), 2, "no duplicate clear event")
		})
	}
}

func TestTransitionSuccessfulRebindDoesNotLogPending(t *testing.T) {
	sink := &rebindEventLogger{Logger: log.Logger}
	previous := log.Logger
	log.Logger = sink
	defer func() { log.Logger = previous }()
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)).Code)
	require.Equal(t, []string{"info:subject_rebind_cleared"}, sink.snapshot())
}

func TestTransitionFailedRemoveReportsPendingOnce(t *testing.T) {
	sink := &rebindEventLogger{Logger: log.Logger}
	previous := log.Logger
	log.Logger = sink
	defer func() { log.Logger = previous }()
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("remove failed")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)).Code)
	token := transitionCurrentToken(srv, subj)
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, removeBody(t, testUID, "B", "B-att")).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj))
	require.Equal(t, []string{"warn:subject_rebind_pending", "warn:subject_rebind_pending"}, sink.snapshot())
	// A replaced pointer cannot report pending or clear the current intent.
	srv.logPendingRebind(subj, &subjectRebind{})
	srv.finishRebind(subj, &subjectRebind{})
	require.Len(t, sink.snapshot(), 2)
	require.Same(t, token, transitionCurrentToken(srv, subj))
}
