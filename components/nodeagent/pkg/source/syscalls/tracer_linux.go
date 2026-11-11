//go:build linux

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

package syscalls

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

//go:embed syscalls_bpfel.o
var syscallBPF []byte

type bpfObjects struct {
	CollectSyscall *ebpf.Program `ebpf:"collect_syscall"`
	TrackedCgroups *ebpf.Map     `ebpf:"tracked_cgroups"`
	Events         *ebpf.Map     `ebpf:"events"`
	LostEvents     *ebpf.Map     `ebpf:"lost_events"`
}

func (o *bpfObjects) Close() error {
	var errs []error
	if o.CollectSyscall != nil {
		errs = append(errs, o.CollectSyscall.Close())
	}
	if o.TrackedCgroups != nil {
		errs = append(errs, o.TrackedCgroups.Close())
	}
	if o.Events != nil {
		errs = append(errs, o.Events.Close())
	}
	if o.LostEvents != nil {
		errs = append(errs, o.LostEvents.Close())
	}
	return errors.Join(errs...)
}

type bpfTracer struct {
	objects  bpfObjects
	link     link.Link
	reader   *ringbuf.Reader
	messages chan kernelMessage
	errors   chan error
	done     chan struct{}
	closing  chan struct{}
	once     sync.Once
}

func newKernelTracer() (kernelTracer, error) {
	// Kernels before 5.11 charge BPF map memory against RLIMIT_MEMLOCK.
	_ = rlimit.RemoveMemlock()
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(syscallBPF))
	if err != nil {
		return nil, fmt.Errorf("read embedded BPF object: %w", err)
	}
	var objects bpfObjects
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		_ = objects.Close()
		return nil, fmt.Errorf("load BPF maps and program: %w", err)
	}
	attached, err := link.Tracepoint("raw_syscalls", "sys_enter", objects.CollectSyscall, nil)
	if err != nil {
		_ = objects.Close()
		return nil, fmt.Errorf("attach raw_syscalls/sys_enter: %w", err)
	}
	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		_ = attached.Close()
		_ = objects.Close()
		return nil, fmt.Errorf("open syscall ring buffer: %w", err)
	}
	tracer := &bpfTracer{objects: objects, link: attached, reader: reader, messages: make(chan kernelMessage, 1024), errors: make(chan error, 1), done: make(chan struct{}), closing: make(chan struct{})}
	go tracer.read()
	return tracer, nil
}

func (t *bpfTracer) Track(cgroupID, handle uint64) error {
	return t.objects.TrackedCgroups.Update(cgroupID, handle, ebpf.UpdateAny)
}

func (t *bpfTracer) Untrack(cgroupID uint64) error {
	err := t.objects.TrackedCgroups.Delete(cgroupID)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	return err
}

func (t *bpfTracer) Flush() error { return t.reader.Flush() }

func (t *bpfTracer) Forget(handle uint64) error {
	err := t.objects.LostEvents.Delete(handle)
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}
	return err
}

func (t *bpfTracer) Messages() <-chan kernelMessage { return t.messages }

func (t *bpfTracer) Errors() <-chan error { return t.errors }

func (t *bpfTracer) Lost() (map[uint64]uint64, error) {
	lost := make(map[uint64]uint64)
	iterator := t.objects.LostEvents.Iterate()
	var handle, count uint64
	for iterator.Next(&handle, &count) {
		lost[handle] = count
	}
	return lost, iterator.Err()
}

func (t *bpfTracer) Close() error {
	var closeErr error
	t.once.Do(func() {
		close(t.closing)
		closeErr = errors.Join(t.link.Close(), t.reader.Close())
		<-t.done
		closeErr = errors.Join(closeErr, t.objects.Close())
	})
	return closeErr
}

func (t *bpfTracer) read() {
	defer close(t.done)
	var record ringbuf.Record
	for {
		err := t.reader.ReadInto(&record)
		if err != nil {
			if errors.Is(err, ringbuf.ErrFlushed) {
				select {
				case t.messages <- kernelMessage{Drained: true}:
				case <-t.closing:
					return
				}
				continue
			}
			if !errors.Is(err, ringbuf.ErrClosed) {
				select {
				case t.errors <- err:
				case <-t.closing:
				}
			}
			return
		}
		event, err := decodeKernelEvent(record.RawSample)
		if err != nil {
			select {
			case t.errors <- err:
			case <-t.closing:
			}
			return
		}
		select {
		case t.messages <- kernelMessage{Event: event}:
		case <-t.closing:
			return
		}
	}
}
