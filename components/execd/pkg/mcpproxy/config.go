// Copyright 2025 The OpenSandbox Authors
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

// Package mcpproxy bridges MCP (Model Context Protocol) clients to upstream
// MCP servers. Clients connect with any MCP SDK over Streamable HTTP at
// /mcp/{name}; execd forwards to the configured upstream, either an HTTP
// (Streamable HTTP) MCP server or a stdio MCP server subprocess. The bridge
// is protocol-aware only where the upstream requires it (stdio); HTTP
// upstreams are proxied as an opaque stream without JSON-RPC inspection.
package mcpproxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// Transport selects the upstream bridge type.
type Transport string

const (
	// TransportHTTP forwards to a Streamable HTTP MCP server.
	TransportHTTP Transport = "http"
	// TransportStdio spawns and bridges a stdio MCP server subprocess.
	TransportStdio Transport = "stdio"
)

// namePattern constrains server names to one path segment without escapes.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ServerConfig describes one upstream MCP server.
type ServerConfig struct {
	// Name is the routing segment under /mcp/. Required, unique.
	Name string `json:"name"`
	// Transport selects the bridge: "http" or "stdio". Required.
	Transport Transport `json:"transport"`

	// URL is the Streamable HTTP endpoint of the upstream, including its
	// path (conventionally "/mcp"). Required for the http transport.
	URL string `json:"url,omitempty"`
	// Headers are set on every upstream request, overriding client values.
	// Use them to hold upstream credentials so they never reach clients.
	Headers map[string]string `json:"headers,omitempty"`

	// Command is the stdio server executable. Required for the stdio
	// transport. Commands come from sandbox image or platform configuration,
	// never from clients.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env is added to the subprocess environment on top of execd's own.
	Env map[string]string `json:"env,omitempty"`
	// CWD is the subprocess working directory; empty inherits execd's.
	CWD string `json:"cwd,omitempty"`
}

// Config is the parsed mcpproxy configuration.
type Config struct {
	Servers []*ServerConfig
	// SSEKeepalive is the idle interval at which HTTP bridges inject an SSE
	// comment line on text/event-stream responses. Zero disables injection.
	SSEKeepalive time.Duration
}

// ParseConfig parses the JSON server list and validates every entry.
// An empty payload yields an empty (disabled) configuration.
func ParseConfig(raw string, sseKeepalive time.Duration) (*Config, error) {
	cfg := &Config{SSEKeepalive: sseKeepalive}
	if raw == "" {
		return cfg, nil
	}

	var servers []*ServerConfig
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		return nil, fmt.Errorf("mcpproxy: parse servers JSON: %w", err)
	}
	cfg.Servers = servers

	seen := make(map[string]struct{}, len(servers))
	for i, s := range servers {
		if s == nil {
			return nil, fmt.Errorf("mcpproxy: server[%d] is null", i)
		}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("mcpproxy: server[%d]: %w", i, err)
		}
		if _, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("mcpproxy: duplicate server name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return cfg, nil
}

// Validate checks one server entry.
func (s *ServerConfig) Validate() error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("invalid name %q (want 1-64 chars: letters, digits, '.', '_', '-')", s.Name)
	}
	switch s.Transport {
	case TransportHTTP:
		u, err := url.Parse(s.URL)
		if err != nil {
			return fmt.Errorf("invalid url %q: %w", s.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("url %q must use http or https", s.URL)
		}
		if u.Host == "" {
			return fmt.Errorf("url %q is missing a host", s.URL)
		}
		if s.Command != "" {
			return fmt.Errorf("http transport must not set command")
		}
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("stdio transport requires command")
		}
		if s.URL != "" {
			return fmt.Errorf("stdio transport must not set url")
		}
	default:
		return fmt.Errorf("unknown transport %q (want %q or %q)", s.Transport, TransportHTTP, TransportStdio)
	}
	return nil
}
