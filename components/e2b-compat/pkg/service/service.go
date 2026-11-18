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

// Package service hosts the HTTP skeleton shared by the adapter and router.
// It serves only the liveness check; no E2B route is registered and no
// upstream request is made.
package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/alibaba/opensandbox/e2b-compat/pkg/config"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	// ShutdownTimeout bounds graceful shutdown after the context is canceled.
	ShutdownTimeout = 10 * time.Second
)

// NewHandler returns a private mux. GET and HEAD on the exact health path
// return 200 (process liveness only, not backend or envd readiness); other
// methods there get 405 and every other path 404.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+config.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n")) // net/http drops the body for HEAD
	})
	// The method-less pattern is less specific, so GET/HEAD still hit the
	// handler above; every other method on the health path gets 405.
	mux.HandleFunc(config.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	return mux
}

// Serve runs handler on ln until ctx is canceled, then shuts down within
// shutdownTimeout. A shutdown triggered by ctx is not an error.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		<-serveErr
		return err
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
