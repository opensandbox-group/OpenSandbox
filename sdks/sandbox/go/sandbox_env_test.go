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

package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const setEnvSuccessPayload = "{\"type\":\"execution_complete\",\"timestamp\":1,\"execution_time\":1}\n\n"

func expectedSetEnvCommand(entry, key string) string {
	quotedEntry := "'" + strings.ReplaceAll(entry, "'", `'\''`) + "'"
	return strings.Join([]string{
		fmt.Sprintf("if [ -z \"${EXECD_ENVS:-}\" ]; then printf '%%s\\n' 'EXECD_ENVS is not set; cannot persist environment variable %s' >&2; exit 1; fi", key),
		`mkdir -p "$(dirname "$EXECD_ENVS")"`,
		fmt.Sprintf("printf '%%s=%%s\\n' %s >> \"$EXECD_ENVS\"", quotedEntry),
	}, "\n")
}

func TestBuildSetEnvCommandEscaping(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		entry string
	}{
		{
			name:  "simple value uses single-quoted form",
			key:   "MY_TOKEN",
			value: "value",
			entry: "MY_TOKEN='value'",
		},
		{
			name:  "dollar backslash and newline stay literal",
			key:   "MY_VAR",
			value: "line1\nline2 $HOME \\path",
			entry: "MY_VAR='line1\nline2 $HOME \\path'",
		},
		{
			name:  "single quote switches to double-quoted form",
			key:   "GREETING",
			value: "it's fine \"quoted\"\ttab",
			entry: `GREETING="it's fine \"quoted\"\ttab"`,
		},
		{
			name:  "equals sign in value is preserved",
			key:   "KV",
			value: "a=b=c",
			entry: "KV='a=b=c'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, err := buildSetEnvCommand(tt.key, tt.value)
			require.NoError(t, err)
			require.Equal(t, expectedSetEnvCommand(tt.entry, tt.key), command)
		})
	}
}

func TestBuildSetEnvCommandRejectsInvalidInput(t *testing.T) {
	for _, key := range []string{"", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B"} {
		_, err := buildSetEnvCommand(key, "value")
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "SetEnv key must match"), "err = %v", err)
	}
	_, err := buildSetEnvCommand("MY_TOKEN", "a\x00b")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "NUL"), "err = %v", err)
}

func TestSandboxSetEnvAppendsEntryViaEnvFile(t *testing.T) {
	var gotCommand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			t.Errorf("expected POST /command, got %s %s", r.Method, r.URL.Path)
		}
		var req RunCommandRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		gotCommand = req.Command

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(setEnvSuccessPayload))
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-setenv", execd: NewExecdClient(srv.URL, "tok")}
	require.NoError(t, sb.SetEnv(context.Background(), "MY_TOKEN", "value"))
	require.Equal(t, expectedSetEnvCommand("MY_TOKEN='value'", "MY_TOKEN"), gotCommand)
}

func TestSandboxSetEnvSurfacesStderrOnFailure(t *testing.T) {
	failurePayload := strings.Join([]string{
		"{\"type\":\"init\",\"text\":\"cmd-1\",\"timestamp\":1}",
		"{\"type\":\"stderr\",\"text\":\"EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN\",\"timestamp\":2}",
		"{\"type\":\"error\",\"error\":{\"ename\":\"CommandExecError\",\"evalue\":\"1\",\"traceback\":[\"exit status 1\"]},\"timestamp\":3}",
		"",
	}, "\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(failurePayload))
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-setenv", execd: NewExecdClient(srv.URL, "tok")}
	err := sb.SetEnv(context.Background(), "MY_TOKEN", "value")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), `SetEnv "MY_TOKEN" failed`), "err = %v", err)
	require.True(t, strings.Contains(err.Error(), "EXECD_ENVS is not set"), "err = %v", err)
}

func TestSandboxSetEnvRejectsInvalidKeysBeforeTransport(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-setenv", execd: NewExecdClient(srv.URL, "tok")}
	for _, key := range []string{"", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B"} {
		err := sb.SetEnv(context.Background(), key, "value")
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "SetEnv key must match"), "err = %v", err)
	}
	require.Equal(t, 0, requests)
}
