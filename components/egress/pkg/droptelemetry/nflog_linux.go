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

//go:build linux

package droptelemetry

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/florianl/go-nflog/v2"
)

func Bind(ctx context.Context, c Config, onPacket func(Packet), onError func(error)) (func(), error) {
	nf, err := nflog.Open(&nflog.Config{Group: c.Group, Copymode: nflog.CopyPacket, Bufsize: 128, QThresh: 1, Timeout: 10})
	if err != nil {
		return nil, err
	}
	receiveCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	var stoppedOnce sync.Once
	signalStopped := func() { stoppedOnce.Do(func() { close(stopped) }) }
	err = nf.RegisterWithErrorFunc(receiveCtx, func(a nflog.Attribute) int {
		signalStopped()
		if receiveCtx.Err() != nil {
			telemetry.SetDropSampler(false, "disabled")
			return 1
		}
		if a.Payload == nil || a.Prefix == nil || !strings.HasPrefix(*a.Prefix, "osb:") {
			return 0
		}
		reason := Reason(strings.TrimSuffix(strings.TrimPrefix(*a.Prefix, "osb:"), "\x00"))
		if !ValidReason(reason) {
			return 0
		}
		if p, ok := ParsePacket(*a.Payload); ok {
			p.Reason = reason
			onPacket(p)
		}
		return 0
	}, func(e error) int {
		signalStopped()
		if receiveCtx.Err() != nil || errors.Is(e, net.ErrClosed) || errors.Is(e, os.ErrClosed) || errors.Is(e, syscall.EBADF) {
			cause := "receive"
			if receiveCtx.Err() != nil {
				cause = "disabled"
			}
			telemetry.SetDropSampler(false, cause)
			cancel()
			return 1
		}
		onError(e)
		var netErr net.Error
		if errors.Is(e, os.ErrDeadlineExceeded) || errors.As(e, &netErr) && netErr.Timeout() {
			cancel()
			return 1
		}
		return 0
	})
	if err != nil {
		cancel()
		_ = nf.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			telemetry.SetDropSampler(false, "disabled")
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			// Callbacks follow go-nflog's background WaitGroup.Add, ordering it before Close's Wait.
			// The timeout lacks that ordering; a deferred unbind after Close may also log a harmless error.
			select {
			case <-stopped:
			case <-timer.C:
				log.Debugf("drop telemetry: receiver close timed out waiting for a callback")
			}
			_ = nf.Close()
		})
	}, nil
}
