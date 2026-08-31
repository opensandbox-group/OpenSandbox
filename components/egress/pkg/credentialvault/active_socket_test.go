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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpGetUnix performs a GET over a unix socket (the addon's transport).
func httpGetUnix(t *testing.T, socketPath, target string) (*http.Response, string) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := client.Get("http://credential-proxy" + target)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// shortSocketPath returns a unix socket path under the system temp dir with a
// short parent the server creates itself (StartActiveSocketServer chmods the
// socket's parent dir to 0o710; sun_path caps at ~104 chars, so
// t.TempDir()-based paths are too long on macOS). The parent dir is registered
// for cleanup: the server's own cleanup removes only the socket file.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("oscv-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "active.sock")
}

func TestStartActiveSocketServerLegacyHandlerUntouched(t *testing.T) {
	socketPath := shortSocketPath(t)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	srv, cleanup, err := StartActiveSocketServer(func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("single-vault"))
	}, socketPath, -1)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, cleanup(shutdownCtx))
	}()

	resp, body := httpGetUnix(t, socketPath, "/credential-vault/_active")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "single-vault", body)

	// GET-only is enforced at the socket server
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err = client.Post("http://credential-proxy/credential-vault/_active", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	_ = srv
}

func TestStartActiveSocketServerRequestAwareDispatch(t *testing.T) {
	socketPath := shortSocketPath(t)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	_, cleanup, err := StartActiveSocketServerRequestAware(func(w http.ResponseWriter, r *http.Request) {
		// the fleet handler dispatch: clientIp -> subject -> vault
		ip := r.URL.Query().Get("clientIp")
		if ip == "" {
			http.Error(w, "clientIp query parameter required", http.StatusBadRequest)
			return
		}
		if ip == "10.0.0.5" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"revision":1}`))
			return
		}
		http.Error(w, "no subject for clientIp", http.StatusNotFound)
	}, socketPath, -1)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, cleanup(shutdownCtx))
	}()

	resp, body := httpGetUnix(t, socketPath, "/credential-vault/_active?clientIp=10.0.0.5")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, `{"revision":1}`, body)

	resp, _ = httpGetUnix(t, socketPath, "/credential-vault/_active?clientIp=10.0.0.99")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, _ = httpGetUnix(t, socketPath, "/credential-vault/_active")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.NotEmpty(t, resp.Status)
}
