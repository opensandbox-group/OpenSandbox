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

// Package api contains the stable extension contracts shared by Node Agent
// Sources, the Pipeline, and Sinks.
package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// RecordKind names one storage format family. Every RecordKind must have a
// registered streamformat.Format before a Sink can persist its records;
// changing the encoding or layout of an existing kind requires a new kind.
type RecordKind string

// Built-in Source names and RecordKinds compiled into Node Agent.
const (
	SourceNameContainerLogs = "container-logs"
	SourceNameSyscalls      = "syscalls"

	RecordKindContainerLog RecordKind = "container-log"
	RecordKindSyscall      RecordKind = "syscall"
)

// Capabilities declares the RecordKinds a Source emits or a Sink accepts. The
// Pipeline refuses to connect a Source and Sink whose kinds do not intersect
// exactly.
type Capabilities struct {
	RecordKinds []RecordKind
}

// Resource is the stable identity of the sandbox a record belongs to. It is
// frozen into every persisted object and must stay identical across one
// stream's whole lifetime.
type Resource struct {
	SandboxID   string `json:"sandbox_id"`
	ClusterName string `json:"k8s.cluster.name"`
	Namespace   string `json:"k8s.namespace.name"`
	PodName     string `json:"k8s.pod.name"`
	PodUID      string `json:"k8s.pod.uid"`
	NodeName    string `json:"k8s.node.name"`
	Container   string `json:"k8s.container.name"`
}

// StreamMetadata contains immutable, format-specific stream identity. Sources
// must keep it stable for every event in a StreamRef.
type StreamMetadata map[string]string

// Clone returns a deep copy of the metadata.
func (m StreamMetadata) Clone() StreamMetadata {
	if m == nil {
		return nil
	}
	clone := make(StreamMetadata, len(m))
	for key, value := range m {
		clone[key] = value
	}
	return clone
}

// Equal reports whether both maps hold the same key/value pairs.
func (m StreamMetadata) Equal(other StreamMetadata) bool {
	if len(m) != len(other) {
		return false
	}
	for key, value := range m {
		otherValue, found := other[key]
		if !found || otherValue != value {
			return false
		}
	}
	return true
}

// Validate rejects empty keys and non-UTF-8 members.
func (m StreamMetadata) Validate() error {
	for key, value := range m {
		if key == "" {
			return errors.New("stream metadata key is empty")
		}
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return fmt.Errorf("stream metadata %q is not valid UTF-8", key)
		}
	}
	return nil
}

// Record is one payload item flowing from a Source through the Pipeline to a
// Sink.
type Record struct {
	Kind       RecordKind        `json:"kind"`
	Timestamp  time.Time         `json:"timestamp"`
	Body       []byte            `json:"body"`
	Resource   Resource          `json:"resource"`
	Attributes map[string]string `json:"attributes"`
}

// StreamRef identifies one append-only object family. A Source must never
// reuse an ID for a different RecordKind.
type StreamRef struct {
	// ID is a stable identity within a Source namespace. A Source must never
	// reuse an ID for a different RecordKind.
	ID   string     `json:"id"`
	Kind RecordKind `json:"kind"`
}

// AckToken returns delivery ownership from the Pipeline to the Source that
// emitted the record. Sources verify identity fields before acting on it.
type AckToken struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	StreamRef StreamRef `json:"stream_ref"`
	Value     []byte    `json:"value"`
}

// EndToken returns stream-finalization ownership from the Pipeline to the
// Source that emitted the StreamEnd. Sources verify identity fields before
// acting on it.
type EndToken struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	StreamRef StreamRef `json:"stream_ref"`
	Value     []byte    `json:"value"`
}

// Clone returns a deep copy of the token; the clone's Value slice is
// independent.
func (t EndToken) Clone() EndToken {
	clone := t
	clone.Value = slices.Clone(t.Value)
	return clone
}

// Equal reports whether both tokens carry identical identity fields and
// values.
func (t EndToken) Equal(other EndToken) bool {
	return t.ID == other.ID && t.Source == other.Source && t.StreamRef == other.StreamRef && slices.Equal(t.Value, other.Value)
}

// AckDisposition tells a Source how a delivered record was handled.
type AckDisposition string

const (
	// AckDelivered means the record reached durable or best-effort storage.
	AckDelivered AckDisposition = "delivered"
	// AckIntentionalDrop means a Pipeline policy (budget, rate limit) dropped
	// the record; Reason carries the machine-readable cause.
	AckIntentionalDrop AckDisposition = "intentional-drop"
)

// DeliveryGuarantee declares what a Sink promises for accepted records.
type DeliveryGuarantee string

const (
	// GuaranteeDurable means an accepted record survives process crashes.
	GuaranteeDurable DeliveryGuarantee = "durable"
	// GuaranteeBestEffort means accepted records may be lost on crash; Sources
	// must keep their own recovery metadata to bound such gaps.
	GuaranteeBestEffort DeliveryGuarantee = "best-effort"
)

// AckResult is one acknowledgement the Pipeline returns to a Source.
type AckResult struct {
	Token       AckToken          `json:"token"`
	Disposition AckDisposition    `json:"disposition"`
	Reason      string            `json:"reason,omitempty"`
	Guarantee   DeliveryGuarantee `json:"guarantee"`
}

// SourceOutcome summarizes coverage quality for a finished stream. Sources
// freeze it into the StreamEnd; Sinks persist it with the finalization marker.
type SourceOutcome struct {
	HadDrops      bool     `json:"had_drops"`
	HadSourceGaps bool     `json:"had_source_gaps"`
	LossReasons   []string `json:"loss_reasons"`
}

// AddLossReason appends reason to reasons unless it is already present.
// Loss reasons stay duplicate-free so outcomes remain comparable and
// finalization markers can require unique members.
func AddLossReason(reasons []string, reason string) []string {
	if !slices.Contains(reasons, reason) {
		return append(reasons, reason)
	}
	return reasons
}

// Clone returns a deep copy of the outcome; the clone's LossReasons slice is
// independent.
func (o SourceOutcome) Clone() SourceOutcome {
	clone := o
	clone.LossReasons = slices.Clone(o.LossReasons)
	return clone
}

// Equal reports whether both outcomes record the same flags and loss reasons.
func (o SourceOutcome) Equal(other SourceOutcome) bool {
	return o.HadDrops == other.HadDrops && o.HadSourceGaps == other.HadSourceGaps && slices.Equal(o.LossReasons, other.LossReasons)
}

// Delivery is one record handed to the Pipeline for storage.
type Delivery struct {
	Record    Record
	StreamRef StreamRef
	Metadata  StreamMetadata
	AckToken  AckToken
	RecordID  string
}

// StreamEnd closes a stream revision. CoverageStartedAt must be a canonical
// UTC RFC3339 timestamp truncated to seconds so Sinks can persist it in
// finalization markers.
type StreamEnd struct {
	StreamRef         StreamRef
	EndToken          EndToken
	Revision          uint64
	CoverageStartedAt time.Time
	Resource          Resource
	Metadata          StreamMetadata
	Outcome           SourceOutcome
}

// SourceEvent is exactly one of Delivery or End; a nil-versus-set pairing of
// both is a Source bug.
type SourceEvent struct {
	Delivery *Delivery
	End      *StreamEnd
}

// Valid reports whether exactly one of Delivery or End is set.
func (e SourceEvent) Valid() bool {
	return (e.Delivery == nil) != (e.End == nil)
}

// Source produces the records of the RecordKinds declared by Capabilities.
type Source interface {
	Capabilities() Capabilities
	// Start launches asynchronous event production and returns after startup.
	// The Source owns the provided channel and closes it exactly once after its
	// producer exits. Closing it before ctx is canceled is a runtime failure.
	Start(context.Context, chan<- SourceEvent) error
	// Acknowledge and AcknowledgeEnd remain usable after Stop until the Pipeline
	// has finished flushing events that were accepted before Stop returned.
	Acknowledge(context.Context, []AckResult) error
	AcknowledgeEnd(context.Context, EndToken) error
	// Stop prevents further event production and waits for the Source-owned
	// event channel to close before returning.
	Stop(context.Context) error
}

// RetryableError classifies whether retrying an operation with unchanged
// inputs and configuration can succeed.
type RetryableError interface {
	error
	Retryable() bool
}

// IsRetryableError reports whether a failed operation may be retried.
// Unclassified errors are conservatively treated as retryable.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var classified RetryableError
	return !errors.As(err, &classified) || classified.Retryable()
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }
func (permanentError) Retryable() bool { return false }

// Permanent marks err as non-retryable while preserving it for errors.Is and
// errors.As. A nil error remains nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// BatchItem is one record inside a Batch, paired with the identity the Source
// needs to acknowledge it.
type BatchItem struct {
	Record   Record
	RecordID string
}

// Batch is one atomic append unit for a single stream. Items share the
// batch's StreamRef, Resource, and Metadata.
type Batch struct {
	StreamRef StreamRef
	Metadata  StreamMetadata
	Items     []BatchItem
}

// FinalizeRequest carries everything a Sink needs to durably close a stream
// revision and publish its finalization marker.
type FinalizeRequest struct {
	FinalizeID        string
	TargetID          string
	StreamRef         StreamRef
	Revision          uint64
	CoverageStartedAt time.Time
	Resource          Resource
	Metadata          StreamMetadata
	Outcome           SourceOutcome
	FinalizedAt       time.Time
}

// Sink stores the RecordKinds declared by Capabilities.
type Sink interface {
	Capabilities() Capabilities
	// Guarantee reports whether Consume'd records survive crashes once Consume
	// returns nil.
	Guarantee() DeliveryGuarantee
	// Consume appends a batch to durable storage. It must be safe to retry a
	// failed call with the same Batch.
	Consume(context.Context, Batch) error
	// Finalize publishes the finalization marker for a completed revision. It
	// must be safe to retry after a partial failure.
	Finalize(context.Context, FinalizeRequest) error
	// Close releases Sink resources after all in-flight work finishes.
	Close(context.Context) error
}
