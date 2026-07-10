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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

type inlineSource struct {
	value string
}

func (s *inlineSource) Type() string                              { return "inline" }
func (s *inlineSource) Resolve(_ context.Context) (string, error) { return s.value, nil }

// inlineSourceFactory creates an inlineSource from the raw JSON "source"
// object. Expected shape: {"type": "inline", "value": "<secret>"}.
// Unknown fields are rejected to match the OpenAPI additionalProperties: false
// contract.
func inlineSourceFactory(raw json.RawMessage) (CredentialSource, error) {
	var src struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&src); err != nil {
		return nil, fmt.Errorf("parse inline credential source: %w", err)
	}
	if src.Value == "" {
		return nil, fmt.Errorf("inline credential value cannot be empty")
	}
	return &inlineSource{value: src.Value}, nil
}
