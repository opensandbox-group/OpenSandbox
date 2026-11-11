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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const setEnvSuccessPayload = "{\"type\":\"execution_complete\",\"timestamp\":1,\"execution_time\":1}\n\n"

// roundTripCases maps key/value to the exact line the snippet must append to
// the env file. Expected lines are hand-written literals.
var roundTripCases = []struct {
	key, value, line string
}{
	{"MY_TOKEN", "value", "MY_TOKEN='value'"},
	{"MY_VAR", "line1\nline2 $HOME \\path", "MY_VAR='line1\nline2 $HOME \\path'"},
	{"KV", "a=b=c", "KV='a=b=c'"},
	{"EMPTY", "", "EMPTY=''"},
	{"GREETING", "it's fine \"quoted\"\ttab", `GREETING="it's fine \"quoted\"\ttab"`},
	{"PATHY", "it's\nC:\\path", `PATHY="it's\nC:\\path"`},
}

func TestBuildSetEnvCommandGolden(t *testing.T) {
	// Hand-written golden literal (not derived from the implementation).
	want := strings.Join([]string{
		"if [ -z \"${EXECD_ENVS:-}\" ]; then printf '%s\\n' " +
			"'EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN' >&2; exit 1; fi",
		`mkdir -p "$(dirname "$EXECD_ENVS")"`,
		`printf '%s\n' 'MY_TOKEN='\''value'\''' >> "$EXECD_ENVS"`,
	}, "\n")

	command, err := buildSetEnvCommand("MY_TOKEN", "value")
	require.NoError(t, err)
	require.Equal(t, want, command)
}

func TestBuildSetEnvCommandRejectsInvalidInput(t *testing.T) {
	for _, key := range []string{"", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B", "A\n"} {
		_, err := buildSetEnvCommand(key, "value")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SetEnv key must match")
	}
	_, err := buildSetEnvCommand("MY_TOKEN", "a\x00b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")

	_, err = buildSetEnvCommand("MY_TOKEN", "a\xffb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UTF-8")
}

// runSetEnvSnippet executes the emitted snippet through /bin/sh with
// EXECD_ENVS pointing at a temp file and returns the appended file content.
// It guards against printf format/argument mismatches that string-comparison
// tests cannot catch.
func runSetEnvSnippet(t *testing.T, command string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell round-trip test")
	}
	envFile := filepath.Join(t.TempDir(), ".env")
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "EXECD_ENVS="+envFile)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "snippet: %s", out)

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	return string(data)
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
	require.Equal(
		t,
		`printf '%s\n' 'MY_TOKEN='\''value'\''' >> "$EXECD_ENVS"`,
		strings.Split(gotCommand, "\n")[2],
	)
}

func TestSandboxSetEnvSnippetRoundTripsWellFormedLines(t *testing.T) {
	for _, tt := range roundTripCases {
		t.Run(fmt.Sprintf("%s", tt.key), func(t *testing.T) {
			command, err := buildSetEnvCommand(tt.key, tt.value)
			require.NoError(t, err)
			require.Equal(t, tt.line+"\n", runSetEnvSnippet(t, command))
		})
	}
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
	assert.Contains(t, err.Error(), `SetEnv "MY_TOKEN" failed`)
	assert.Contains(t, err.Error(), "EXECD_ENVS is not set")
}

func TestSandboxSetEnvTreatsDroppedStreamAsFailure(t *testing.T) {
	// Stream ends after init only: no execution_complete and no error event,
	// so the append was never confirmed and SetEnv must not report success.
	incompletePayload := "{\"type\":\"init\",\"text\":\"cmd-1\",\"timestamp\":1}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(incompletePayload))
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-setenv", execd: NewExecdClient(srv.URL, "tok")}
	err := sb.SetEnv(context.Background(), "MY_TOKEN", "value")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `SetEnv "MY_TOKEN" failed`)
}

func TestSandboxSetEnvRejectsInvalidKeysBeforeTransport(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-setenv", execd: NewExecdClient(srv.URL, "tok")}
	for _, key := range []string{"", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B", "A\n"} {
		err := sb.SetEnv(context.Background(), key, "value")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SetEnv key must match")
	}
	require.Equal(t, 0, requests)
}
