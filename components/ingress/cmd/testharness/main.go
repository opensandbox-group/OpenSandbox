// Copyright 2025 Alibaba Group Holding Ltd.
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

//go:build testharness

// Test-only harness — not part of the shipped binary. Wires proxy.NewProxy
// against a mock provider so external clients (curl, HAProxy, real browsers)
// can drive the exact WebSocket relay code under test without needing
// Kubernetes. Enable with `go build -tags testharness`.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/alibaba/opensandbox/ingress/pkg/proxy"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type mockProvider struct{}

func (mockProvider) Start(context.Context) error { return nil }
func (mockProvider) GetEndpoint(string) (*sandbox.EndpointInfo, error) {
	return &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}, nil
}
func (p mockProvider) ResolveEndpoint(_ context.Context, _ sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	return p.GetEndpoint("")
}

func main() {
	port := flag.Int("port", 18800, "proxy port")
	flag.Parse()
	proxy.Logger = slogger.MustNew(slogger.Config{Level: "info"})
	rp := proxy.NewProxy(context.Background(), mockProvider{}, proxy.ModeHeader, nil, nil, nil)
	mux := http.NewServeMux()
	mux.Handle("/", rp)
	log.Printf("test-harness ingress listening on :%d", *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), mux); err != nil {
		log.Fatal(err)
	}
}
