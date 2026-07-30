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

package credentialvault

import (
	"context"
	"encoding/json"
	"fmt"
)

// CredentialSource resolves credential material from a configured source.
// Implementations must be safe for concurrent use.
type CredentialSource interface {
	// Type returns the source discriminator (e.g. "inline").
	Type() string
	// Resolve returns the plaintext credential value.
	// Context allows future providers to perform network calls with
	// timeout/cancellation support.
	Resolve(ctx context.Context) (string, error)
}

// CredentialSourceFactory creates a CredentialSource from raw JSON source
// configuration. The raw bytes are the full "source" object from the API
// request (including the "type" field).
type CredentialSourceFactory func(raw json.RawMessage) (CredentialSource, error)

// SourceRegistry maps source type discriminators to their factories.
type SourceRegistry struct {
	factories map[string]CredentialSourceFactory
}

// NewSourceRegistry creates a registry with the built-in inline provider
// pre-registered.
func NewSourceRegistry() *SourceRegistry {
	r := &SourceRegistry{
		factories: make(map[string]CredentialSourceFactory),
	}
	r.Register("inline", inlineSourceFactory)
	return r
}

// Register adds a factory for the given source type. Panics if the type is
// already registered.
func (r *SourceRegistry) Register(sourceType string, factory CredentialSourceFactory) {
	if _, exists := r.factories[sourceType]; exists {
		panic(fmt.Sprintf("credential source type %q already registered", sourceType))
	}
	r.factories[sourceType] = factory
}

// Create extracts the "type" discriminator from raw JSON and delegates to the
// matching factory. Returns an error for unknown or missing types.
func (r *SourceRegistry) Create(raw json.RawMessage) (CredentialSource, error) {
	sourceType, err := extractSourceType(raw)
	if err != nil {
		return nil, err
	}
	factory, ok := r.factories[sourceType]
	if !ok {
		return nil, fmt.Errorf("unsupported credential source type %q", sourceType)
	}
	return factory(raw)
}

// SupportedTypes returns all registered source type names.
func (r *SourceRegistry) SupportedTypes() []string {
	types := make([]string, 0, len(r.factories))
	for t := range r.factories {
		types = append(types, t)
	}
	return types
}

func extractSourceType(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "inline", nil
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("parse credential source: %w", err)
	}
	if envelope.Type == "" {
		return "inline", nil
	}
	return envelope.Type, nil
}
