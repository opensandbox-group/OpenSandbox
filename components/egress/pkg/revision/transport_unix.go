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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxIPCResponseBytes = 4096

var (
	ErrInvalidTransport         = errors.New("invalid revision IPC input")
	ErrTransportUnavailable     = errors.New("revision IPC unavailable")
	ErrTransportRejected        = errors.New("revision IPC request rejected")
	ErrInvalidTransportResponse = errors.New("invalid revision IPC response")
)

type ipcEnvelope struct {
	Revision Identity `json:"revision"`
	Payload  []byte   `json:"payload"`
}

type ipcCommand struct {
	Revision Identity `json:"revision"`
}

type ipcResponse struct {
	Revision *Identity `json:"revision"`
}

func decodeStrictObject(data []byte, fields map[string]func(*json.Decoder) error) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalidTransportResponse
	}
	seen := make(map[string]struct{}, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		decode, known := fields[name]
		if err != nil || !ok || !known {
			return ErrInvalidTransportResponse
		}
		if _, duplicate := seen[name]; duplicate {
			return ErrInvalidTransportResponse
		}
		seen[name] = struct{}{}
		if err := decode(decoder); err != nil {
			return ErrInvalidTransportResponse
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != len(fields) {
		return ErrInvalidTransportResponse
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidTransportResponse
	}
	return nil
}

func decodeIPCResponse(data []byte) (*ipcResponse, error) {
	var raw json.RawMessage
	if err := decodeStrictObject(data, map[string]func(*json.Decoder) error{
		"revision": func(decoder *json.Decoder) error { return decoder.Decode(&raw) },
	}); err != nil {
		return nil, err
	}
	response := &ipcResponse{}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return response, nil
	}
	identity := Identity{}
	if err := decodeStrictObject(raw, map[string]func(*json.Decoder) error{
		"controlGeneration": func(decoder *json.Decoder) error { return decoder.Decode(&identity.ControlGeneration) },
		"subjectGeneration": func(decoder *json.Decoder) error { return decoder.Decode(&identity.SubjectGeneration) },
		"decisionEpoch":     func(decoder *json.Decoder) error { return decoder.Decode(&identity.DecisionEpoch) },
		"vaultRevision":     func(decoder *json.Decoder) error { return decoder.Decode(&identity.VaultRevision) },
		"policyEpoch":       func(decoder *json.Decoder) error { return decoder.Decode(&identity.PolicyEpoch) },
		"digest":            func(decoder *json.Decoder) error { return decoder.Decode(&identity.Digest) },
	}); err != nil || !validIPCIdentity(identity) {
		return nil, ErrInvalidTransportResponse
	}
	response.Revision = &identity
	return response, nil
}

// UnixTransport sends revision transactions over a caller-provisioned private
// Unix socket. It presents a bearer token for receiver-side client
// authentication; generation fields provide the independent stale-session
// fence. It never includes remote response bodies in errors because those
// bodies are not trusted to be credential-free.
type UnixTransport struct {
	client *http.Client
	token  string
	limit  int
}

var _ Transport = (*UnixTransport)(nil)

func (*UnixTransport) String() string { return "revision.UnixTransport" }

func (*UnixTransport) GoString() string { return "revision.UnixTransport{}" }

// NewSessionToken returns 256 bits encoded without padding for one IPC session.
func NewSessionToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", ErrTransportUnavailable
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// NewUnixTransport constructs a transport without dialing. The socket server
// must use the same token and snapshot byte limit.
func NewUnixTransport(socketPath, sessionToken string, maxSnapshotBytes int) (*UnixTransport, error) {
	if !filepath.IsAbs(socketPath) || !validSessionToken(sessionToken) || maxSnapshotBytes <= 0 {
		return nil, ErrInvalidTransport
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 8 * 1024,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &UnixTransport{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token: sessionToken,
		limit: maxSnapshotBytes,
	}, nil
}

func validSessionToken(value string) bool {
	if len(value) < 32 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validIPCIdentity(r Identity) bool {
	validGeneration := func(value string) bool {
		return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 128
	}
	if !validGeneration(r.ControlGeneration) || !validGeneration(r.SubjectGeneration) ||
		r.DecisionEpoch < 1 || r.VaultRevision < 0 || r.PolicyEpoch < 0 || len(r.Digest) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(r.Digest)
	return err == nil && len(decoded) == 32 && strings.ToLower(r.Digest) == r.Digest
}

func (t *UnixTransport) Prepare(ctx context.Context, revision Identity, payload []byte) (Identity, error) {
	if len(payload) > t.limit {
		return Identity{}, ErrInvalidTransport
	}
	if len(payload) == 0 {
		payload = []byte{}
	}
	return t.command(ctx, http.MethodPost, "/v1/revisions/prepare", ipcEnvelope{Revision: revision, Payload: payload})
}

func (t *UnixTransport) Commit(ctx context.Context, revision Identity) (Identity, error) {
	return t.command(ctx, http.MethodPost, "/v1/revisions/commit", ipcCommand{Revision: revision})
}

func (t *UnixTransport) Abort(ctx context.Context, revision Identity) (Identity, error) {
	return t.command(ctx, http.MethodPost, "/v1/revisions/abort", ipcCommand{Revision: revision})
}

func (t *UnixTransport) Readback(ctx context.Context) (*Identity, error) {
	response, err := t.do(ctx, http.MethodGet, "/v1/revisions/active", nil)
	if err != nil {
		return nil, err
	}
	return response.Revision, nil
}

func (t *UnixTransport) command(ctx context.Context, method, path string, value any) (Identity, error) {
	var revision Identity
	switch typed := value.(type) {
	case ipcEnvelope:
		revision = typed.Revision
	case ipcCommand:
		revision = typed.Revision
	default:
		return Identity{}, ErrInvalidTransport
	}
	if !validIPCIdentity(revision) {
		return Identity{}, ErrInvalidTransport
	}
	response, err := t.do(ctx, method, path, value)
	if err != nil {
		return Identity{}, err
	}
	if response.Revision == nil {
		return Identity{}, ErrInvalidTransportResponse
	}
	return *response.Revision, nil
}

func (t *UnixTransport) do(ctx context.Context, method, path string, value any) (*ipcResponse, error) {
	if ctx == nil {
		return nil, ErrInvalidTransport
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, ErrInvalidTransport
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://revision-ipc"+path, body)
	if err != nil {
		return nil, ErrInvalidTransport
	}
	request.Header.Set("Authorization", "Bearer "+t.token)
	request.Header.Set("Content-Type", "application/json")
	request.Close = true
	response, err := t.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransportUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrTransportRejected
	}
	if response.Header.Get("Content-Type") != "application/json" {
		return nil, ErrInvalidTransportResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxIPCResponseBytes+1))
	if err != nil || len(data) > maxIPCResponseBytes {
		return nil, ErrInvalidTransportResponse
	}
	return decodeIPCResponse(data)
}
