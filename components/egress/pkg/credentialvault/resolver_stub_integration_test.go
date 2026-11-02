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
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

// Integration tests against the frozen OSEP-0025 contract stub
// (tests/resolver/resolver.py). Assertions key on the deterministic
// sha256(subject|credential|binding|authority|path) signature, not wall-clock.

const stubResolverReadyPrefix = "test resolver ready: "

func stubResolverPath(t *testing.T, relative string) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "components", "egress", "tests", "resolver", relative))
	require.NoError(t, err)
	require.FileExists(t, script)
	return script
}

func stubResolverScript(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for resolver stub integration tests")
	}
	return stubResolverPath(t, "resolver.py")
}

func stubResolverTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	certPEM, err := os.ReadFile(stubResolverPath(t, filepath.Join("fixtures", "test-localhost-cert.pem")))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM), "fixture certificate must parse")
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

type stubResolverProcess struct {
	cmd      *exec.Cmd
	endpoint string
}

// startStubResolver launches one resolver.py process; returns the ready
// line endpoint ("uds://<path>" or "https://HOST:PORT").
func startStubResolver(t *testing.T, stubEnv []string, args ...string) *stubResolverProcess {
	t.Helper()
	script := stubResolverScript(t)
	python, err := exec.LookPath("python3")
	require.NoError(t, err)

	cmd := exec.Command(python, append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), stubEnv...)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	cmd.Stdout = io.Discard
	require.NoError(t, cmd.Start())

	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if endpoint, ok := strings.CutPrefix(scanner.Text(), stubResolverReadyPrefix); ok {
				ready <- strings.TrimSpace(endpoint)
			}
		}
	}()

	var process *stubResolverProcess
	select {
	case endpoint := <-ready:
		process = &stubResolverProcess{cmd: cmd, endpoint: endpoint}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("resolver stub did not become ready in time")
	}

	t.Cleanup(func() {
		_ = process.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = process.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = process.cmd.Process.Kill()
			_ = process.cmd.Wait()
		}
	})
	return process
}

// shortTempDir avoids AF_UNIX sun_path overflow on long test-name temp dirs.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "osbr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func stubSignature(subject, credential, binding, authority, path string) string {
	source := strings.Join([]string{subject, credential, binding, authority, path}, "|")
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

// requireStubHappyDecision asserts the stub's deterministic header set and
// the merged redaction completeness for the shared test input.
func requireStubHappyDecision(t *testing.T, decision *ResolverDecision) {
	t.Helper()
	input := resolverTestInput()
	expectedSignature := stubSignature(input.Subject, input.Credential, input.Binding, input.Destination.Authority, input.Request.Path)
	require.Equal(t, map[string]string{
		"authorization":    "Bearer test-" + input.Credential,
		"x-test-signature": expectedSignature,
		"x-test-subject":   input.Subject,
	}, decision.Headers)
	require.Equal(t, map[string]struct{}{
		"authorization": {}, "x-test-signature": {}, "x-test-subject": {},
	}, decision.Suppress)

	// R9: derived variants cover every header value even without resolver
	// redactions.
	for _, value := range decision.Headers {
		for _, variant := range substitutionRedactionVariants(value) {
			require.Contains(t, decision.Redactions, variant)
		}
	}
	require.Contains(t, decision.Redactions, expectedSignature)
}

func TestStubIntegrationUDSHappyPath(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "r.sock")
	stub := startStubResolver(t, nil, "--uds", socketPath)
	require.True(t, strings.HasPrefix(stub.endpoint, "uds://"), "unexpected endpoint %q", stub.endpoint)

	client, err := NewResolverClient(ResolverClientOptions{Endpoint: stub.endpoint, Timeout: 5 * time.Second, MaxConcurrency: 4})
	require.NoError(t, err)
	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	requireStubHappyDecision(t, decision)
}

func TestStubIntegrationUDSHeaderlessFrame(t *testing.T) {
	// nil headers must serialize as {} — the stub 400s on null.
	socketPath := filepath.Join(shortTempDir(t), "r.sock")
	stub := startStubResolver(t, nil, "--uds", socketPath)

	client, err := NewResolverClient(ResolverClientOptions{Endpoint: stub.endpoint, Timeout: 5 * time.Second, MaxConcurrency: 4})
	require.NoError(t, err)
	input := resolverTestInput()
	input.Request.Headers = nil
	decision, err := client.Resolve(context.Background(), input)
	require.NoError(t, err)
	requireStubHappyDecision(t, decision)
}

func TestStubIntegrationUDSIncompleteRedactions(t *testing.T) {
	// Redactions reported for one header; derivation still covers all (R9).
	socketPath := filepath.Join(shortTempDir(t), "r.sock")
	stub := startStubResolver(t, nil, "--uds", socketPath, "--incomplete-redactions")

	client, err := NewResolverClient(ResolverClientOptions{Endpoint: stub.endpoint, Timeout: 5 * time.Second, MaxConcurrency: 4})
	require.NoError(t, err)
	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	requireStubHappyDecision(t, decision)
}

func stubHTTPSArgs(t *testing.T, port int) []string {
	t.Helper()
	return []string{
		"--https", fmt.Sprintf("127.0.0.1:%d", port),
		"--tls-cert", stubResolverPath(t, filepath.Join("fixtures", "test-localhost-cert.pem")),
		"--tls-key", stubResolverPath(t, filepath.Join("fixtures", "test-localhost-key.pem")),
	}
}

func TestStubIntegrationHTTPSAuthOriginTable(t *testing.T) {
	// Pre-bind the port: the stub reads its auth table at startup.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	origin := fmt.Sprintf("https://127.0.0.1:%d", port)

	// One env table drives both the stub's expected token and the client.
	authEntry := constants.EnvCredentialResolverAuth + "=" + origin + "=resolver-token-1"
	stub := startStubResolver(t, []string{authEntry}, stubHTTPSArgs(t, port)...)
	require.Equal(t, origin, stub.endpoint)

	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        stub.endpoint,
		Timeout:         5 * time.Second,
		MaxConcurrency:  4,
		Auth:            map[string]string{origin: "resolver-token-1"},
		TLSClientConfig: stubResolverTLSConfig(t),
	})
	require.NoError(t, err)
	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	requireStubHappyDecision(t, decision)
}

func TestStubIntegrationHTTPSUnmatchedOriginGetsNoToken(t *testing.T) {
	// The client holds only another origin's token; the stub must 401.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	origin := fmt.Sprintf("https://127.0.0.1:%d", port)

	authEntry := constants.EnvCredentialResolverAuth + "=" + origin + "=resolver-token-1"
	stub := startStubResolver(t, []string{authEntry}, stubHTTPSArgs(t, port)...)

	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        stub.endpoint,
		Timeout:         5 * time.Second,
		MaxConcurrency:  4,
		Auth:            map[string]string{"https://unrelated-resolver.internal:8443": "resolver-token-1"},
		TLSClientConfig: stubResolverTLSConfig(t),
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	resolverErr := requireKind(t, err, ResolverRejected)
	require.Equal(t, http.StatusUnauthorized, resolverErr.Status)
}

func TestStubIntegrationTimeoutClass(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "r.sock")
	stub := startStubResolver(t, nil, "--uds", socketPath, "--delay", "2000")
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: stub.endpoint, Timeout: 300 * time.Millisecond, MaxConcurrency: 4})
	require.NoError(t, err)
	start := time.Now()
	_, err = client.Resolve(context.Background(), resolverTestInput())
	requireKind(t, err, ResolverTimeout)
	require.Less(t, time.Since(start), 1500*time.Millisecond)
}

func TestStubIntegrationFailOnceThenRecover(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "r.sock")
	stub := startStubResolver(t, nil, "--uds", socketPath, "--fail", "once")
	client, err := NewResolverClient(ResolverClientOptions{Endpoint: stub.endpoint, Timeout: 5 * time.Second, MaxConcurrency: 4})
	require.NoError(t, err)

	_, err = client.Resolve(context.Background(), resolverTestInput())
	resolverErr := requireKind(t, err, ResolverRejected)
	require.Equal(t, http.StatusServiceUnavailable, resolverErr.Status)

	// The refusal is per-request: the next resolve succeeds deterministically.
	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	requireStubHappyDecision(t, decision)
}
