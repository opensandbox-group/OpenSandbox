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
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"
)

const maxSubjectMetadataBytes = 8192

// SubjectTarget identifies one explicitly registered installation-only owner.
// The trusted caller supplies a fresh generation for each subject incarnation.
type SubjectTarget struct {
	SubjectKey        string `json:"subjectKey"`
	SubjectGeneration string `json:"subjectGeneration"`
}

func validSubjectString(value string, limit int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit
}

func validSubjectTarget(target SubjectTarget) bool {
	return validSubjectString(target.SubjectKey, 256) && validSubjectString(target.SubjectGeneration, 128)
}

// subjectBodyLimit checks the base64 expansion without overflowing int.
func subjectBodyLimit(snapshotBytes int) (int, error) {
	if snapshotBytes <= 0 {
		return 0, ErrInvalidTransport
	}
	groups := snapshotBytes / 3
	if snapshotBytes%3 != 0 {
		groups++
	}
	if groups > (int(^uint(0)>>1)-maxSubjectMetadataBytes)/4 {
		return 0, ErrInvalidTransport
	}
	return groups*4 + maxSubjectMetadataBytes, nil
}

type subjectIPCClient struct {
	client *http.Client
	token  string
}

func newSubjectIPCClient(socketPath, sessionToken string) (*subjectIPCClient, error) {
	base, err := NewUnixTransport(socketPath, sessionToken, 1)
	if err != nil {
		return nil, err
	}
	return &subjectIPCClient{client: base.client, token: base.token}, nil
}

// post never propagates response bodies or transport error details.
func (c *subjectIPCClient) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	if ctx == nil {
		return 0, nil, ErrInvalidTransport
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://revision-ipc"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, ErrInvalidTransport
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Close = true
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, ErrTransportUnavailable
	}
	defer response.Body.Close()
	if err := ctx.Err(); err != nil {
		return response.StatusCode, nil, err
	}
	if values := response.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		return response.StatusCode, nil, ErrInvalidTransportResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxIPCResponseBytes+1))
	if ctx.Err() != nil {
		return response.StatusCode, nil, ctx.Err()
	}
	if err != nil || len(data) > maxIPCResponseBytes {
		return response.StatusCode, nil, ErrInvalidTransportResponse
	}
	return response.StatusCode, data, nil
}

// SubjectUnixTransport binds a Transport to one session and exact subject. It
// does not register the target and cannot be retargeted after construction.
// This v2 adapter is installation-only; no production launch uses it.
type SubjectUnixTransport struct {
	ipc       *subjectIPCClient
	control   string
	target    SubjectTarget
	limit     int
	bodyLimit int
}

var _ Transport = (*SubjectUnixTransport)(nil)

func (*SubjectUnixTransport) String() string   { return "revision.SubjectUnixTransport" }
func (*SubjectUnixTransport) GoString() string { return "revision.SubjectUnixTransport{}" }

// NewSubjectUnixTransport constructs a transport without dialing. The server
// must have the same control generation, token, and snapshot limit.
func NewSubjectUnixTransport(socketPath, sessionToken, controlGeneration string, target SubjectTarget, maxSnapshotBytes int) (*SubjectUnixTransport, error) {
	if !validSubjectString(controlGeneration, 128) || !validSubjectTarget(target) {
		return nil, ErrInvalidTransport
	}
	bodyLimit, err := subjectBodyLimit(maxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	ipc, err := newSubjectIPCClient(socketPath, sessionToken)
	if err != nil {
		return nil, err
	}
	return &SubjectUnixTransport{ipc: ipc, control: controlGeneration, target: target, limit: maxSnapshotBytes, bodyLimit: bodyLimit}, nil
}

func (t *SubjectUnixTransport) Prepare(ctx context.Context, identity Identity, payload []byte) (Identity, error) {
	if len(payload) > t.limit {
		return Identity{}, ErrInvalidTransport
	}
	if payload == nil {
		payload = []byte{}
	}
	return t.command(ctx, "prepare", identity, struct {
		Target   SubjectTarget `json:"target"`
		Revision Identity      `json:"revision"`
		Payload  []byte        `json:"payload"`
	}{t.target, identity, payload})
}

func (t *SubjectUnixTransport) Commit(ctx context.Context, identity Identity) (Identity, error) {
	return t.identityCommand(ctx, "commit", identity)
}

func (t *SubjectUnixTransport) Abort(ctx context.Context, identity Identity) (Identity, error) {
	return t.identityCommand(ctx, "abort", identity)
}

func (t *SubjectUnixTransport) identityCommand(ctx context.Context, operation string, identity Identity) (Identity, error) {
	return t.command(ctx, operation, identity, struct {
		Target   SubjectTarget `json:"target"`
		Revision Identity      `json:"revision"`
	}{t.target, identity})
}

func (t *SubjectUnixTransport) command(ctx context.Context, operation string, identity Identity, value any) (Identity, error) {
	if !validIPCIdentity(identity) || identity.ControlGeneration != t.control || identity.SubjectGeneration != t.target.SubjectGeneration {
		return Identity{}, ErrInvalidTransport
	}
	response, err := t.do(ctx, "/v2/revisions/"+operation, value)
	if err != nil {
		return Identity{}, err
	}
	if response.Revision == nil {
		return Identity{}, ErrInvalidTransportResponse
	}
	return *response.Revision, nil
}

func (t *SubjectUnixTransport) Readback(ctx context.Context) (*Identity, error) {
	response, err := t.do(ctx, "/v2/revisions/active", struct {
		Target            SubjectTarget `json:"target"`
		ControlGeneration string        `json:"controlGeneration"`
	}{t.target, t.control})
	if err != nil {
		return nil, err
	}
	return response.Revision, nil
}

func (t *SubjectUnixTransport) do(ctx context.Context, path string, value any) (*ipcResponse, error) {
	if ctx == nil {
		return nil, ErrInvalidTransport
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > t.bodyLimit {
		return nil, ErrInvalidTransport
	}
	status, data, err := t.ipc.post(ctx, path, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, ErrTransportRejected
	}
	response, err := decodeSubjectRevisionResponse(data)
	if err != nil {
		return nil, err
	}
	if r := response.Revision; r != nil && (r.ControlGeneration != t.control || r.SubjectGeneration != t.target.SubjectGeneration) {
		return nil, ErrInvalidTransportResponse
	}
	return response, nil
}

// encoding/json replaces malformed UTF-8 and unpaired UTF-16 escapes. V2 must
// reject both rather than silently changing identities received on the wire.
func validSubjectJSON(data []byte) bool {
	if !utf8.Valid(data) || !json.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' {
				continue
			}
			i++
			if data[i] != 'u' {
				continue
			}
			code, _ := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			i += 4
			if code >= 0xdc00 && code <= 0xdfff {
				return false
			}
			if code >= 0xd800 && code <= 0xdbff {
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}

func subjectField(value any) func(*json.Decoder) error {
	return func(decoder *json.Decoder) error {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ErrInvalidTransportResponse
		}
		return json.Unmarshal(raw, value)
	}
}

func decodeSubjectRevisionResponse(data []byte) (*ipcResponse, error) {
	if !validSubjectJSON(data) {
		return nil, ErrInvalidTransportResponse
	}
	var raw json.RawMessage
	if err := decodeStrictObject(data, map[string]func(*json.Decoder) error{
		"revision": func(decoder *json.Decoder) error { return decoder.Decode(&raw) },
	}); err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &ipcResponse{}, nil
	}
	var identity Identity
	if err := decodeStrictObject(raw, map[string]func(*json.Decoder) error{
		"controlGeneration": subjectField(&identity.ControlGeneration),
		"subjectGeneration": subjectField(&identity.SubjectGeneration),
		"decisionEpoch":     subjectField(&identity.DecisionEpoch),
		"vaultRevision":     subjectField(&identity.VaultRevision),
		"policyEpoch":       subjectField(&identity.PolicyEpoch),
		"digest":            subjectField(&identity.Digest),
	}); err != nil || !validIPCIdentity(identity) {
		return nil, ErrInvalidTransportResponse
	}
	return &ipcResponse{Revision: &identity}, nil
}
