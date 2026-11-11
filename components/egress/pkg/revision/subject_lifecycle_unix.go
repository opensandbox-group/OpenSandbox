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

package revision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
)

var (
	ErrLifecycleSequenceExhausted = errors.New("subject lifecycle sequence exhausted; new session required")
	ErrLifecycleSequenceConflict  = errors.New("subject lifecycle status conflicts with pending command")
)

// SubjectUnloadResult describes installation-owner closure only. There are no
// admitted transports in this owner; terminal is not proof of live socket drain.
type SubjectUnloadResult struct {
	RoutingRevoked       bool   `json:"routingRevoked"`
	TransportObligations string `json:"transportObligations"`
	Completion           string `json:"completion"`
}

// LifecycleResult is a closed, credential-free result for one consumed command.
type LifecycleResult struct {
	HTTPStatus int                  `json:"httpStatus"`
	Outcome    string               `json:"outcome"`
	Unload     *SubjectUnloadResult `json:"unload"`
}

// LifecycleReceipt confirms that this exact operation/raw command was consumed.
// A non-200 Result is a confirmed failure and still retires the pending sequence.
type LifecycleReceipt struct {
	LifecycleSequence int64           `json:"lifecycleSequence"`
	Operation         string          `json:"operation"`
	CommandDigest     string          `json:"commandDigest"`
	Result            LifecycleResult `json:"result"`
}

type subjectLifecycleCommand struct {
	ControlGeneration string        `json:"controlGeneration"`
	LifecycleSequence int64         `json:"lifecycleSequence"`
	Target            SubjectTarget `json:"target"`
}

type pendingSubjectLifecycle struct {
	sequence  int64
	operation string
	digest    string
	raw       []byte
}

// SubjectLifecycleClient is the sole sequence allocator for a fresh process
// session. Never reconstruct it with an already-used control generation/token.
// It serializes lifecycle I/O and retains at most one frozen command. Revision
// Coordinators operate independently and do not consume lifecycle sequences.
//
// Register, Unload and Reconcile callers must supply contexts with deadlines:
// the HTTP client has no implicit timeout. The mutex is held across network I/O,
// and waiting for it is not context-aware. Concurrent calls, including Pending,
// therefore wait for the in-flight operation to finish; each operation's caller
// is responsible for bounding that operation with its own deadline.
type SubjectLifecycleClient struct {
	mu      sync.Mutex
	ipc     *subjectIPCClient
	control string
	last    int64
	pending *pendingSubjectLifecycle
}

func (*SubjectLifecycleClient) String() string   { return "revision.SubjectLifecycleClient" }
func (*SubjectLifecycleClient) GoString() string { return "revision.SubjectLifecycleClient{}" }

// NewSubjectLifecycleClient constructs a fresh sequence owner without dialing.
func NewSubjectLifecycleClient(socketPath, sessionToken, controlGeneration string) (*SubjectLifecycleClient, error) {
	if !validSubjectString(controlGeneration, 128) {
		return nil, ErrInvalidTransport
	}
	ipc, err := newSubjectIPCClient(socketPath, sessionToken)
	if err != nil {
		return nil, err
	}
	return &SubjectLifecycleClient{ipc: ipc, control: controlGeneration}, nil
}

// Register creates known/uninstalled state; it does not install an empty snapshot.
// ErrTransportRejected accompanies a confirmed failure receipt. ErrIndeterminate
// requires Reconcile before any later lifecycle command can be allocated.
// The caller must supply a context with a deadline; see SubjectLifecycleClient.
func (c *SubjectLifecycleClient) Register(ctx context.Context, target SubjectTarget) (LifecycleReceipt, error) {
	return c.command(ctx, "register", target)
}

// Unload closes exactly this installation owner. A confirmed subject_failed
// receipt retires its sequence but leaves the remote owner fenced; another
// explicit Unload may retry closure with a new sequence.
// The caller must supply a context with a deadline; see SubjectLifecycleClient.
func (c *SubjectLifecycleClient) Unload(ctx context.Context, target SubjectTarget) (LifecycleReceipt, error) {
	return c.command(ctx, "unload", target)
}

// Pending reports whether an earlier command still needs exact reconciliation.
// Register and Unload cannot replace it, even with the same target/operation.
// Pending waits for any in-flight lifecycle call; it is not a nonblocking probe.
func (c *SubjectLifecycleClient) Pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending != nil
}

func (c *SubjectLifecycleClient) command(ctx context.Context, operation string, target SubjectTarget) (LifecycleReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		return LifecycleReceipt{}, ErrIndeterminate
	}
	if ctx == nil || !validSubjectTarget(target) {
		return LifecycleReceipt{}, ErrInvalidTransport
	}
	if err := ctx.Err(); err != nil {
		return LifecycleReceipt{}, err
	}
	if c.last == math.MaxInt64 {
		return LifecycleReceipt{}, ErrLifecycleSequenceExhausted
	}
	sequence := c.last + 1
	raw, err := json.Marshal(subjectLifecycleCommand{c.control, sequence, target})
	if err != nil || len(raw) > maxSubjectMetadataBytes {
		return LifecycleReceipt{}, ErrInvalidTransport
	}
	// Allocate only after all local validation. Retries use raw verbatim.
	c.pending = &pendingSubjectLifecycle{sequence: sequence, operation: operation, digest: subjectLifecycleDigest(operation, raw), raw: raw}
	receipt, err := c.sendLocked(ctx, true)
	if receipt == nil {
		return LifecycleReceipt{}, err
	}
	return *receipt, err
}

// Reconcile confirms only the existing pending command, never a new operation.
// At most one exact retry follows status showing the preceding sequence. An
// unresolved result retains pending and requires a later call with a new context;
// repeated authentication, framing, and configuration errors do not busy-loop.
// The caller must supply a context with a deadline; see SubjectLifecycleClient.
func (c *SubjectLifecycleClient) Reconcile(ctx context.Context) (*LifecycleReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		return nil, nil
	}
	if ctx == nil {
		return nil, ErrInvalidTransport
	}
	return c.reconcileLocked(ctx, true)
}

func subjectLifecycleDigest(operation string, raw []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("opensandbox.subject-lifecycle.v2\x00"))
	_, _ = hash.Write([]byte(operation))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *SubjectLifecycleClient) sendLocked(ctx context.Context, mayRetry bool) (*LifecycleReceipt, error) {
	pending := c.pending
	status, data, err := c.ipc.post(ctx, "/v2/subjects/"+pending.operation, pending.raw)
	if err == nil && status == http.StatusOK {
		receipt, parseErr := decodeLifecycleRecord(data, "lifecycleSequence", false)
		if parseErr == nil && receipt.Result.HTTPStatus == status && c.matches(receipt) {
			return c.finishLocked(receipt)
		}
	}
	// A non-200 response, even a valid receipt, is never enough to infer whether
	// the server consumed a sequence. Status is the common recovery path.
	return c.reconcileLocked(ctx, mayRetry)
}

func (c *SubjectLifecycleClient) reconcileLocked(ctx context.Context, mayRetry bool) (*LifecycleReceipt, error) {
	if ctx.Err() != nil {
		return nil, lifecycleUnresolved(ctx)
	}
	body, _ := json.Marshal(struct {
		ControlGeneration string `json:"controlGeneration"`
	}{c.control})
	status, data, err := c.ipc.post(ctx, "/v2/subjects/lifecycle-status", body)
	if err != nil || status != http.StatusOK {
		return nil, lifecycleUnresolved(ctx)
	}
	record, err := decodeLifecycleRecord(data, "lastSequence", true)
	if err != nil {
		return nil, ErrIndeterminate
	}
	if record.LifecycleSequence == c.pending.sequence {
		if !c.matches(record) {
			return nil, ErrLifecycleSequenceConflict
		}
		return c.finishLocked(record)
	}
	if record.LifecycleSequence != c.pending.sequence-1 {
		return nil, ErrLifecycleSequenceConflict
	}
	if mayRetry {
		return c.sendLocked(ctx, false)
	}
	return nil, lifecycleUnresolved(ctx)
}

func lifecycleUnresolved(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrIndeterminate, err)
	}
	return ErrIndeterminate
}

func (c *SubjectLifecycleClient) matches(record *LifecycleReceipt) bool {
	return record.LifecycleSequence == c.pending.sequence && record.Operation == c.pending.operation && record.CommandDigest == c.pending.digest
}

func (c *SubjectLifecycleClient) finishLocked(receipt *LifecycleReceipt) (*LifecycleReceipt, error) {
	c.last = receipt.LifecycleSequence
	c.pending = nil
	if receipt.Result.HTTPStatus != http.StatusOK {
		return receipt, ErrTransportRejected
	}
	return receipt, nil
}

func decodeLifecycleRecord(data []byte, sequenceKey string, allowInitial bool) (*LifecycleReceipt, error) {
	if len(data) > maxIPCResponseBytes || !validSubjectJSON(data) {
		return nil, ErrInvalidTransportResponse
	}
	record := &LifecycleReceipt{}
	var operation, digest, result json.RawMessage
	if err := decodeStrictObject(data, map[string]func(*json.Decoder) error{
		sequenceKey:     subjectField(&record.LifecycleSequence),
		"operation":     func(d *json.Decoder) error { return d.Decode(&operation) },
		"commandDigest": func(d *json.Decoder) error { return d.Decode(&digest) },
		"result":        func(d *json.Decoder) error { return d.Decode(&result) },
	}); err != nil {
		return nil, err
	}
	isNull := func(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
	if allowInitial && record.LifecycleSequence == 0 && isNull(operation) && isNull(digest) && isNull(result) {
		return record, nil
	}
	if record.LifecycleSequence < 1 || isNull(operation) || isNull(digest) || isNull(result) ||
		json.Unmarshal(operation, &record.Operation) != nil || json.Unmarshal(digest, &record.CommandDigest) != nil ||
		(record.Operation != "register" && record.Operation != "unload") || !validLifecycleDigest(record.CommandDigest) {
		return nil, ErrInvalidTransportResponse
	}
	decoded, err := decodeLifecycleResult(result, record.Operation)
	if err != nil {
		return nil, err
	}
	record.Result = *decoded
	return record, nil
}

func validLifecycleDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func decodeLifecycleResult(data []byte, operation string) (*LifecycleResult, error) {
	result := &LifecycleResult{}
	var unload json.RawMessage
	if err := decodeStrictObject(data, map[string]func(*json.Decoder) error{
		"httpStatus": subjectField(&result.HTTPStatus),
		"outcome":    subjectField(&result.Outcome),
		"unload":     func(d *json.Decoder) error { return d.Decode(&unload) },
	}); err != nil {
		return nil, err
	}
	completion := ""
	expectedStatus := 0
	switch result.Outcome {
	case "invalid_target":
		expectedStatus = http.StatusBadRequest
	case "registered", "already_registered":
		if operation == "register" {
			expectedStatus = http.StatusOK
		}
	case "subject_conflict":
		if operation == "register" {
			expectedStatus = http.StatusConflict
		}
	case "subject_capacity_exhausted":
		if operation == "register" {
			expectedStatus = http.StatusServiceUnavailable
		}
	case "subject_generation_mismatch":
		if operation == "unload" {
			expectedStatus = http.StatusConflict
		}
	case "unloaded", "already_absent":
		if operation == "unload" {
			expectedStatus, completion = http.StatusOK, "terminal"
		}
	case "subject_failed":
		expectedStatus = http.StatusServiceUnavailable
		if operation == "unload" {
			completion = "fenced"
		}
	}
	if expectedStatus == 0 || result.HTTPStatus != expectedStatus {
		return nil, ErrInvalidTransportResponse
	}
	if completion == "" {
		if !bytes.Equal(bytes.TrimSpace(unload), []byte("null")) {
			return nil, ErrInvalidTransportResponse
		}
		return result, nil
	}
	closed := &SubjectUnloadResult{}
	if err := decodeStrictObject(unload, map[string]func(*json.Decoder) error{
		"routingRevoked":       subjectField(&closed.RoutingRevoked),
		"transportObligations": subjectField(&closed.TransportObligations),
		"completion":           subjectField(&closed.Completion),
	}); err != nil || !closed.RoutingRevoked || closed.TransportObligations != "none" || closed.Completion != completion {
		return nil, ErrInvalidTransportResponse
	}
	result.Unload = closed
	return result, nil
}
