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

package mcpproxy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/version"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// stdioIdleSessionTTL closes a bridged MCP session after this much client
// inactivity; closing the session tears down the upstream subprocess.
const stdioIdleSessionTTL = 10 * time.Minute

// stdioBridgeProtocolVersions are the protocol versions the bridge
// advertises. Excluding the 2026-07-28+ generation steers discover-capable
// clients (Go SDK) onto the legacy initialize path — the bridge itself
// exposes no features, so discovery would advertise an empty tool surface —
// while still accepting every official SDK's MCP-Protocol-Version header
// (the java SDK sends its default before negotiation).
var stdioBridgeProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// stdioBridge bridges one stdio MCP server. The Streamable HTTP protocol
// surface (sessions, SSE, DELETE termination, idle timeouts) is provided by
// the official Go SDK's StreamableHTTPHandler; this type owns per-session
// subprocesses and forwards MCP requests to them.
//
// Process model: one subprocess per MCP session. stdio servers are
// single-session by design; sharing one process across sessions would mix
// their state. Session lifecycle (client DELETE, idle timeout, child exit)
// always tears the subprocess down.
//
// Session state lives in the receiving middleware (keyed by ServerSession
// ID), not in the handler's getServer callback: the SDK invokes getServer
// more than once per request (once for the protocol-version check), so it
// must stay side-effect free.
type stdioBridge struct {
	cfg     *ServerConfig
	server  *mcp.Server
	handler http.Handler

	mu       sync.Mutex
	sessions map[string]*stdioSession // keyed by MCP session ID
	closed   bool
}

// stdioSession pairs one MCP HTTP session with its upstream subprocess.
type stdioSession struct {
	bridge        *stdioBridge
	id            string
	serverSession *mcp.ServerSession

	mu            sync.Mutex
	clientSession *mcp.ClientSession
	ended         bool
	cancel        context.CancelFunc
	endOnce       sync.Once
}

func newStdioBridge(cfg *ServerConfig) (*stdioBridge, error) {
	b := &stdioBridge{cfg: cfg, sessions: make(map[string]*stdioSession)}
	b.server = mcp.NewServer(
		&mcp.Implementation{Name: "opensandbox-execd-mcp-bridge", Version: version.Version},
		&mcp.ServerOptions{
			SupportedProtocolVersions: stdioBridgeProtocolVersions,
			// The bridge forwards whatever the upstream offers, but the
			// upstream only spawns after initialize — so the bridge must
			// advertise the feature surfaces it can bridge up front. If the
			// upstream lacks one, its method-not-found error passes through.
			Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{},
				Prompts:   &mcp.PromptCapabilities{},
				Resources: &mcp.ResourceCapabilities{},
			},
			InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
				if sess, ok := req.GetSession().(*mcp.ServerSession); ok {
					if s := b.sessionByID(sess.ID()); s != nil {
						s.initUpstream()
					}
				}
			},
		},
	)
	b.server.AddReceivingMiddleware(b.middleware())
	b.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return b.server },
		&mcp.StreamableHTTPOptions{SessionTimeout: stdioIdleSessionTTL})
	return b, nil
}

// ServeHTTP implements the /mcp/{name} endpoint for stdio upstreams.
func (b *stdioBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.handler.ServeHTTP(w, r)
}

// middleware lazily tracks every MCP session and forwards bridged request
// methods to the session's stdio subprocess. Notifications and unbridged
// methods fall through to the SDK's own handlers.
func (b *stdioBridge) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			serverSession, ok := req.GetSession().(*mcp.ServerSession)
			if ok {
				s := b.sessionByID(serverSession.ID())
				if s == nil {
					s = b.registerSession(serverSession)
				}
				if s == nil {
					// The bridge is shutting down; sessions are closing.
					return nil, &jsonrpc.Error{
						Code:    jsonrpc.CodeInternalError,
						Message: "mcp bridge is shutting down",
					}
				}
				result, err := s.forward(ctx, method, req.GetParams())
				if errors.Is(err, errNotBridged) {
					return next(ctx, method, req)
				}
				return result, err
			}
			return next(ctx, method, req)
		}
	}
}

// errNotBridged marks methods the bridge does not handle itself.
var errNotBridged = errors.New("mcpproxy: method not bridged")

// bridgedMethods are the MCP request methods forwarded to the stdio
// subprocess. Everything else (initialize, ping, notifications, resource
// subscriptions, logging levels) is served by the bridge server itself.
var bridgedMethods = map[string]struct{}{
	"tools/list":               {},
	"tools/call":               {},
	"prompts/list":             {},
	"prompts/get":              {},
	"resources/list":           {},
	"resources/templates/list": {},
	"resources/read":           {},
	"completion/complete":      {},
}

// forward dispatches one bridged method to the subprocess client session.
func (s *stdioSession) forward(ctx context.Context, method string, params mcp.Params) (mcp.Result, error) {
	if _, ok := bridgedMethods[method]; !ok {
		return nil, errNotBridged
	}
	cs := s.upstream()
	if cs == nil {
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: "mcp upstream process is not available",
		}
	}

	switch method {
	case "tools/list":
		return cs.ListTools(ctx, params.(*mcp.ListToolsParams))
	case "tools/call":
		// The server receives raw arguments; the client side takes any
		// JSON-marshalable value, so forward the raw message unchanged.
		raw := params.(*mcp.CallToolParamsRaw)
		return cs.CallTool(ctx, &mcp.CallToolParams{
			Meta:      raw.Meta,
			Name:      raw.Name,
			Arguments: raw.Arguments,
		})
	case "prompts/list":
		return cs.ListPrompts(ctx, params.(*mcp.ListPromptsParams))
	case "prompts/get":
		return cs.GetPrompt(ctx, params.(*mcp.GetPromptParams))
	case "resources/list":
		return cs.ListResources(ctx, params.(*mcp.ListResourcesParams))
	case "resources/templates/list":
		return cs.ListResourceTemplates(ctx, params.(*mcp.ListResourceTemplatesParams))
	case "resources/read":
		return cs.ReadResource(ctx, params.(*mcp.ReadResourceParams))
	case "completion/complete":
		return cs.Complete(ctx, params.(*mcp.CompleteParams))
	default:
		return nil, errNotBridged
	}
}

// upstream returns the child client session, or nil before initialization
// or after a spawn failure.
func (s *stdioSession) upstream() *mcp.ClientSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientSession
}

// registerSession starts tracking an MCP session. The watcher tears the
// subprocess down whenever the web session ends, including sessions that
// never reach initialization (e.g. ephemeral discover sessions, which the
// SDK handler closes right after answering).
func (b *stdioBridge) registerSession(serverSession *mcp.ServerSession) *stdioSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.sessions[serverSession.ID()]; s != nil {
		return s
	}
	if b.closed {
		return nil
	}
	s := &stdioSession{bridge: b, id: serverSession.ID(), serverSession: serverSession}
	b.sessions[s.id] = s
	go func() {
		_ = serverSession.Wait()
		s.end()
	}()
	return s
}

// initUpstream spawns the subprocess once the session is initialized and
// starts the child watcher.
func (s *stdioSession) initUpstream() {
	s.mu.Lock()
	if s.ended || s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	sess, err := s.connectUpstream(ctx)
	s.mu.Lock()
	if err != nil {
		// Keep cancel so the failed exec context is released on end().
		s.mu.Unlock()
		log.Error("mcpproxy: upstream %s: start subprocess: %v", s.bridge.cfg.Name, err)
		return
	}
	s.clientSession = sess
	s.mu.Unlock()
	log.Info("mcpproxy: upstream %s: subprocess started for session %s", s.bridge.cfg.Name, s.id)

	go func() {
		_ = sess.Wait()
		log.Info("mcpproxy: upstream %s: subprocess for session %s exited", s.bridge.cfg.Name, s.id)
		s.end()
	}()
}

// connectUpstream launches the configured command and performs the MCP
// initialize handshake with it.
func (s *stdioSession) connectUpstream(ctx context.Context) (*mcp.ClientSession, error) {
	cfg := s.bridge.cfg
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.CWD
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(cfg.Env)...)
	}
	configureProcessGroup(cmd)

	client := mcp.NewClient(
		&mcp.Implementation{Name: "opensandbox-execd-mcp-bridge", Version: version.Version},
		nil,
	)
	return client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
}

// end tears the session down exactly once: close the child client (stdin
// close, then graceful terminate), cancel the process context, and
// unregister.
func (s *stdioSession) end() {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.ended = true
		cs := s.clientSession
		cancel := s.cancel
		s.mu.Unlock()
		if cs != nil {
			_ = cs.Close()
		}
		if cancel != nil {
			cancel()
		}
		s.bridge.removeSession(s.id)
	})
}

// sessionByID returns the live session for an MCP session ID.
func (b *stdioBridge) sessionByID(id string) *stdioSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

func (b *stdioBridge) removeSession(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
}

// Close terminates every subprocess and rejects new sessions.
func (b *stdioBridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	sessions := make([]*stdioSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.sessions = make(map[string]*stdioSession)
	b.mu.Unlock()
	for _, s := range sessions {
		s.end()
	}
}

// envSlice flattens an env map for exec.Cmd.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
