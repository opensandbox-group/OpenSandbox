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

package log

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	egressLoggerName = "opensandbox.egress"
	outputSyncLimit  = 100 * time.Millisecond
)

var (
	errOutputClosed   = errors.New("egress log output is closed")
	errOutputTimedOut = errors.New("egress log output flush timed out")
)

type outputRecord struct {
	data       []byte
	diagnostic bool // Zap's internal error output, admitted in the ordinary lane.
}

type outputLane struct {
	name                   string
	queue                  chan outputRecord
	byteLimit, queuedBytes int
	accepted, completed    uint64
	dropped, handledDrops  uint64
	changed                chan struct{} // allocated on demand by Sync waiters
}

// Output owns one physical worker and two independently bounded admission lanes.
// Logging returns after encoding and admission (or an accounted overflow drop),
// not after a physical write. The loggers' Sync methods wait up to 100 ms for
// prior work in their own lane; this is not a durability guarantee.
type Output struct {
	ordinary, l4             outputLane
	ordinaryLogger, l4Logger slogger.Logger
	wake, stop, done         chan struct{}

	mu     sync.Mutex // admission and progress only; never held across destination I/O
	closed bool
	result error // first actual destination failure
}

type outputDestinations struct {
	out, errors zapcore.WriteSyncer
	close       func() error
}

// NewOutput opens the configured destinations before starting their sole worker.
func NewOutput(cfg slogger.Config, fields ...slogger.Field) (*Output, error) {
	outputs, err := slogger.OpenOutputs(cfg)
	if err != nil {
		return nil, err
	}
	return newOutput(cfg, outputDestinations{out: outputs.Out, errors: outputs.Errors, close: outputs.Close}, fields...), nil
}

func newOutput(cfg slogger.Config, destinations outputDestinations, fields ...slogger.Field) *Output {
	o := &Output{
		ordinary: outputLane{name: "ordinary", queue: make(chan outputRecord, 256), byteLimit: 1 << 20},
		l4:       outputLane{name: "l4", queue: make(chan outputRecord, 4096), byteLimit: 4 << 20},
		wake:     make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	errs := laneWriter{owner: o, lane: &o.ordinary, diagnostic: true}
	o.ordinaryLogger = slogger.NewWithWriter(cfg, laneWriter{owner: o, lane: &o.ordinary}, errs).Named(egressLoggerName).With(fields...)
	o.l4Logger = slogger.NewWithWriter(cfg, laneWriter{owner: o, lane: &o.l4}, errs).Named(egressLoggerName).With(fields...)

	// Encode contextual fields now, before returning their ownership to callers.
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoder := zapcore.NewJSONEncoder(encoderCfg)
	for _, f := range fields {
		zap.Any(f.Key, f.Value).AddTo(encoder)
	}
	go o.run(destinations, encoder)
	return o
}

// OrdinaryLogger returns the logger for enforcement and lifecycle diagnostics.
func (o *Output) OrdinaryLogger() slogger.Logger { return o.ordinaryLogger }

// L4Logger returns the logger with separate capacity for L4 observations.
func (o *Output) L4Logger() slogger.Logger { return o.l4Logger }

// Close ends both admissions. A timed-out caller does not close the sinks:
// the worker retains ownership until it can finish its writes and close them.
func (o *Output) Close(ctx context.Context) error {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.stop)
	}
	o.mu.Unlock()

	select {
	case <-o.done:
		return o.result
	default:
	}
	select {
	case <-o.done:
		return o.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

type laneWriter struct {
	owner      *Output
	lane       *outputLane
	diagnostic bool
}

func (w laneWriter) Write(p []byte) (int, error) {
	o, lane := w.owner, w.lane
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errOutputClosed
	}
	if len(lane.queue) == cap(lane.queue) || len(p) > lane.byteLimit-lane.queuedBytes {
		lane.dropped++
		select {
		case o.wake <- struct{}{}:
		default:
		}
		// Lossy admission is not a physical write failure and must not trigger
		// Zap to emit another diagnostic on this same producer path.
		return len(p), nil
	}
	data := make([]byte, len(p))
	copy(data, p) // Zap recycles the encoder buffer as soon as Write returns.
	lane.queuedBytes += len(data)
	lane.accepted++
	lane.queue <- outputRecord{data: data, diagnostic: w.diagnostic}
	return len(p), nil
}

func (w laneWriter) Sync() error {
	if w.diagnostic {
		// Zap calls ErrorOutput.Sync inline when it encounters an internal
		// error. Only an explicit logger Sync may wait for lane progress.
		return nil
	}
	o, lane := w.owner, w.lane
	o.mu.Lock()
	accepted, dropped := lane.accepted, lane.dropped
	if lane.completed == accepted && lane.handledDrops == dropped {
		err := o.result
		o.mu.Unlock()
		return err
	}
	timer := time.NewTimer(outputSyncLimit)
	defer timer.Stop()
	for {
		if lane.changed == nil {
			lane.changed = make(chan struct{})
		}
		changed := lane.changed
		o.mu.Unlock()
		select {
		case <-changed:
			o.mu.Lock()
			if lane.completed >= accepted && lane.handledDrops >= dropped {
				err := o.result
				o.mu.Unlock()
				return err
			}
		case <-timer.C:
			return errOutputTimedOut
		}
	}
}

// notify runs under the admission/progress lock.
func (lane *outputLane) notify() {
	if lane.changed != nil {
		close(lane.changed)
		lane.changed = nil
	}
}

func (o *Output) run(destinations outputDestinations, encoder zapcore.Encoder) {
	defer close(o.done)
	defer func() { o.remember(destinations.close()) }()
	stop := o.stop
	for {
		if stop == nil && len(o.ordinary.queue) == 0 && len(o.l4.queue) == 0 {
			o.reportGaps(destinations, encoder)
			return
		}
		select {
		case r := <-o.ordinary.queue:
			o.write(destinations, encoder, &o.ordinary, r)
		case r := <-o.l4.queue:
			o.write(destinations, encoder, &o.l4, r)
		case <-o.wake:
			o.reportGaps(destinations, encoder)
		case <-stop:
			stop = nil
		}
	}
}

func (o *Output) write(destinations outputDestinations, encoder zapcore.Encoder, lane *outputLane, r outputRecord) {
	o.mu.Lock()
	lane.queuedBytes -= len(r.data)
	o.mu.Unlock()

	if r.diagnostic {
		o.remember(writeRecord(destinations.errors, r.data))
	} else if err := writeRecord(destinations.out, r.data); err != nil {
		o.reportFailure(destinations.errors, encoder, lane.name, "record", err)
	}

	o.mu.Lock()
	lane.completed++
	lane.notify()
	o.mu.Unlock()
}

func (o *Output) reportGaps(destinations outputDestinations, encoder zapcore.Encoder) {
	o.mu.Lock()
	ordinary, l4 := o.ordinary.dropped, o.l4.dropped
	o.mu.Unlock()
	o.reportGap(destinations, encoder, &o.ordinary, ordinary)
	o.reportGap(destinations, encoder, &o.l4, l4)
}

func (o *Output) reportGap(destinations outputDestinations, encoder zapcore.Encoder, lane *outputLane, total uint64) {
	// Only this worker advances handledDrops. These are rejected encoded
	// records, not rejected NFLOG copies or failed physical writes.
	if total == lane.handledDrops {
		return
	}
	err := writeDiagnostic(destinations.out, encoder, zapcore.WarnLevel, "egress log output queue dropped records",
		zap.String("opensandbox.event", "egress.log.output_gap"),
		zap.String("egress.log.lane", lane.name),
		zap.Uint64("egress.log.dropped_records", total-lane.handledDrops),
		zap.Uint64("egress.log.dropped_records_total", total))
	if err != nil {
		o.reportFailure(destinations.errors, encoder, lane.name, "gap", err)
	}
	o.mu.Lock()
	lane.handledDrops = total
	lane.notify()
	o.mu.Unlock()
}

func (o *Output) reportFailure(out zapcore.WriteSyncer, encoder zapcore.Encoder, lane, operation string, failure error) {
	o.remember(failure)
	// Direct reporting by the worker only, without another diagnostic if the
	// error sink also fails. The original error is already latched.
	_ = writeDiagnostic(out, encoder, zapcore.ErrorLevel, "egress log output write failed",
		zap.String("opensandbox.event", "egress.log.output_error"),
		zap.String("egress.log.lane", lane),
		zap.String("egress.log.output_operation", operation), zap.Error(failure))
}

func (o *Output) remember(err error) {
	if err == nil {
		return
	}
	o.mu.Lock()
	if o.result == nil {
		o.result = err
	}
	o.mu.Unlock()
}

func writeDiagnostic(out zapcore.WriteSyncer, encoder zapcore.Encoder, level zapcore.Level, msg string, fields ...zapcore.Field) error {
	buf, err := encoder.EncodeEntry(zapcore.Entry{Level: level, Time: time.Now(), LoggerName: egressLoggerName, Message: msg}, fields)
	if err != nil {
		return err
	}
	defer buf.Free()
	return writeRecord(out, buf.Bytes())
}

func writeRecord(out zapcore.WriteSyncer, p []byte) error {
	n, err := out.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}
