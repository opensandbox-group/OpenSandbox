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

package credentialvault

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExchangeSourceFactoryAcceptsSupportedEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"unix:///run/spi/spi.sock",
		"https://header-resolver.internal",
		"https://resolver.internal:8443",
		"https://resolver.internal/prefix",
	} {
		t.Run(endpoint, func(t *testing.T) {
			source, err := NewSourceRegistry().Create(mustMarshal(map[string]string{"type": "exchange", "endpoint": endpoint}))
			require.NoError(t, err)
			require.Equal(t, "exchange", source.Type())
		})
	}
}

func TestExchangeSourceFactoryRejects(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"empty endpoint", `{"type":"exchange","endpoint":""}`},
		{"http scheme", `{"type":"exchange","endpoint":"http://resolver.internal"}`},
		{"bare host", `{"type":"exchange","endpoint":"resolver.internal:8443"}`},
		{"unix relative path", `{"type":"exchange","endpoint":"unix://spi.sock"}`},
		{"unix empty path", `{"type":"exchange","endpoint":"unix:///"}`},
		{"unknown field", `{"type":"exchange","endpoint":"unix:///run/spi.sock","value":"secret"}`},
		{"missing endpoint", `{"type":"exchange"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSourceRegistry().Create(json.RawMessage(tc.source))
			require.Error(t, err)
			require.Contains(t, err.Error(), "exchange credential")
		})
	}
}

func TestExchangeSourceResolveNeverTouchesTheNetwork(t *testing.T) {
	source, err := exchangeSourceFactory(mustMarshal(map[string]string{"type": "exchange", "endpoint": "unix:///run/spi/spi.sock"}))
	require.NoError(t, err)
	value, err := source.Resolve(context.Background())
	require.Error(t, err)
	require.Empty(t, value)
	require.Contains(t, err.Error(), "cannot be resolved inline")
}
