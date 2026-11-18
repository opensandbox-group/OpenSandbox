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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

func StartActiveSocketServer(
	activeHandler func(http.ResponseWriter),
	socketPath string,
	socketGID int,
) (*http.Server, func(context.Context) error, error) {
	if activeHandler == nil {
		return nil, nil, fmt.Errorf("active credential vault handler is required")
	}
	return startActiveSocketServer(func(w http.ResponseWriter, _ *http.Request) { activeHandler(w) }, nil, socketPath, socketGID, nil)
}

// StartActiveSocketServerRequestAware passes the request to active-vault
// handlers. Sidecar handlers inspect conditional snapshot headers; fast-sandbox
// handlers additionally dispatch clientIp (source IP -> subject -> snapshot).
// resolveHandler serves POST /credential-vault/_resolve; nil mounts the route
// forbidden.
// proxyPolicyHandler optionally authorizes explicit proxy destinations over the
// same private socket. It is never exposed on the public policy API.
func StartActiveSocketServerRequestAware(
	activeHandler func(http.ResponseWriter, *http.Request),
	resolveHandler http.Handler,
	socketPath string,
	socketGID int,
	proxyPolicyHandler ...http.Handler,
) (*http.Server, func(context.Context) error, error) {
	if activeHandler == nil {
		return nil, nil, fmt.Errorf("active credential vault handler is required")
	}
	var proxyPolicy http.Handler
	if len(proxyPolicyHandler) > 0 {
		proxyPolicy = proxyPolicyHandler[0]
	}
	return startActiveSocketServer(activeHandler, resolveHandler, socketPath, socketGID, proxyPolicy)
}

func startActiveSocketServer(
	activeHandler func(http.ResponseWriter, *http.Request),
	resolveHandler http.Handler,
	socketPath string,
	socketGID int,
	proxyPolicyHandler http.Handler,
) (*http.Server, func(context.Context) error, error) {
	if socketPath == "" {
		return nil, nil, fmt.Errorf("socket path is required")
	}
	if !filepath.IsAbs(socketPath) {
		return nil, nil, fmt.Errorf("socket path must be absolute")
	}

	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create socket parent directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o710); err != nil {
		return nil, nil, fmt.Errorf("create socket directory: %w", err)
	}
	if socketGID >= 0 {
		if err := os.Chown(dir, os.Geteuid(), socketGID); err != nil {
			return nil, nil, fmt.Errorf("set socket directory ownership: %w", err)
		}
	}
	if err := os.Chmod(dir, 0o710); err != nil {
		return nil, nil, fmt.Errorf("set socket directory mode: %w", err)
	}

	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}
	if socketGID >= 0 {
		if err := os.Chown(socketPath, os.Geteuid(), socketGID); err != nil {
			_ = listener.Close()
			_ = os.Remove(socketPath)
			return nil, nil, fmt.Errorf("set socket ownership: %w", err)
		}
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, nil, fmt.Errorf("set socket mode: %w", err)
	}

	mux := http.NewServeMux()
	if proxyPolicyHandler != nil {
		mux.Handle("/egress/authorize", proxyPolicyHandler)
	}
	mux.HandleFunc("/credential-vault/_active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		activeHandler(w, r)
	})
	mux.HandleFunc("/credential-vault/_resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if resolveHandler == nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		resolveHandler.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		_ = os.Remove(socketPath)
		return nil, nil, err
	default:
	}

	cleanup := func(ctx context.Context) error {
		shutdownErr := srv.Shutdown(ctx)
		removeErr := os.Remove(socketPath)
		if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
			return shutdownErr
		}
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	}
	return srv, cleanup, nil
}
