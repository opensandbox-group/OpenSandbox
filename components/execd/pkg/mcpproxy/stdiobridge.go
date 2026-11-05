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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/internal/version"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
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
	childCmd      *exec.Cmd
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
				Tools:       &mcp.ToolCapabilities{},
				Prompts:     &mcp.PromptCapabilities{},
				Resources:   &mcp.ResourceCapabilities{},
				Completions: &mcp.CompletionCapabilities{},
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
					var err error
					s, err = b.registerSession(serverSession)
					if err != nil {
						// Shutting down, or the per-bridge session cap hit.
						return nil, err
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

// errBridgeClosed marks session registration on a bridge that is closing.
var errBridgeClosed = &jsonrpc.Error{
	Code:    jsonrpc.CodeInternalError,
	Message: "mcp bridge is shutting down",
}

// errTooManySessions marks registration beyond maxSessionsPerBridge.
var errTooManySessions = &jsonrpc.Error{
	Code:    jsonrpc.CodeInternalError,
	Message: "mcp bridge session limit reached; close sessions or remove the server and re-register",
}

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

// badParams reports a params shape the bridge did not expect (an SDK
// upgrade changed a method's params type); the request degrades to a
// JSON-RPC error instead of a panic.
func badParams(method string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInternalError,
		Message: "mcp bridge received unexpected params for " + method,
	}
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
		p, ok := params.(*mcp.ListToolsParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.ListTools(ctx, p)
	case "tools/call":
		// The server receives raw arguments; the client side takes any
		// JSON-marshalable value, so forward the raw message unchanged.
		raw, ok := params.(*mcp.CallToolParamsRaw)
		if !ok {
			return nil, badParams(method)
		}
		return cs.CallTool(ctx, &mcp.CallToolParams{
			Meta:      raw.Meta,
			Name:      raw.Name,
			Arguments: raw.Arguments,
		})
	case "prompts/list":
		p, ok := params.(*mcp.ListPromptsParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.ListPrompts(ctx, p)
	case "prompts/get":
		p, ok := params.(*mcp.GetPromptParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.GetPrompt(ctx, p)
	case "resources/list":
		p, ok := params.(*mcp.ListResourcesParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.ListResources(ctx, p)
	case "resources/templates/list":
		p, ok := params.(*mcp.ListResourceTemplatesParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.ListResourceTemplates(ctx, p)
	case "resources/read":
		p, ok := params.(*mcp.ReadResourceParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.ReadResource(ctx, p)
	case "completion/complete":
		p, ok := params.(*mcp.CompleteParams)
		if !ok {
			return nil, badParams(method)
		}
		return cs.Complete(ctx, p)
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

// maxSessionsPerBridge caps concurrent stdio sessions per upstream. Each
// session owns a subprocess, so this is an accident brake for runaway
// clients, not a security boundary.
const maxSessionsPerBridge = 64

// registerSession starts tracking an MCP session. The watcher tears the
// subprocess down whenever the web session ends, including sessions that
// never reach initialization (e.g. ephemeral discover sessions, which the
// SDK handler closes right after answering).
func (b *stdioBridge) registerSession(serverSession *mcp.ServerSession) (*stdioSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.sessions[serverSession.ID()]; s != nil {
		return s, nil
	}
	if b.closed {
		return nil, errBridgeClosed
	}
	if len(b.sessions) >= maxSessionsPerBridge {
		return nil, errTooManySessions
	}
	s := &stdioSession{bridge: b, id: serverSession.ID(), serverSession: serverSession}
	b.sessions[s.id] = s
	go func() {
		_ = serverSession.Wait()
		s.end()
	}()
	return s, nil
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
	// connectUpstream reads s.childCmd; reserve the spawn slot first so a
	// concurrent call cannot double-spawn (s.cancel already set above).
	s.mu.Unlock()

	sess, cmd, err := s.connectUpstream(ctx)
	s.mu.Lock()
	s.childCmd = cmd
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
		// The SDK's transport owns exec.Cmd.Wait; once it has the exit
		// status, drop the reaper adoption so the consumed exit cannot
		// leave a stale owned entry behind.
		runtime.ReleaseManagedChild(cmd)
		s.end()
	}()
}

// connectUpstream launches the configured command and performs the MCP
// initialize handshake with it.
func (s *stdioSession) connectUpstream(ctx context.Context) (*mcp.ClientSession, *exec.Cmd, error) {
	cfg := s.bridge.cfg
	env := childEnviron(cfg.Env)
	// exec.CommandContext resolves the executable against execd's own PATH
	// at construction; resolve against the child's merged PATH instead so
	// commands shipped via the runtime binding, EXECD_ENVS, or the
	// registration env (venv/npm bin) are found.
	cmd := exec.CommandContext(ctx, resolveChildCommand(cfg.Command, env, cfg.CWD), cfg.Args...)
	cmd.Dir = cfg.CWD
	cmd.Env = env
	configureProcessGroup(cmd)

	// stdio children are workload processes: run them through the same
	// managed path (hardening floor, init reaper) as command and PTY
	// launches. The SDK starts the command itself, so the rewrite happens
	// before Connect and the child is adopted right after.
	release, err := runtime.PrepareManagedChild(cmd)
	if err != nil {
		return nil, nil, err
	}

	client := mcp.NewClient(
		&mcp.Implementation{Name: "opensandbox-execd-mcp-bridge", Version: version.Version},
		nil,
	)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	release()
	if err != nil {
		return nil, nil, err
	}
	runtime.AdoptManagedChild(cmd)
	return sess, cmd, nil
}

// end tears the session down exactly once: close the child client (stdin
// close, then graceful terminate), cancel the process context, close the
// inbound HTTP session (so clients see 404 and recover instead of talking
// to a dead bridge), and unregister.
func (s *stdioSession) end() {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.ended = true
		cs := s.clientSession
		cancel := s.cancel
		serverSession := s.serverSession
		s.mu.Unlock()
		if cs != nil {
			_ = cs.Close()
		}
		if cancel != nil {
			cancel()
		}
		if serverSession != nil {
			_ = serverSession.Close()
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

// childEnviron builds the subprocess environment on the same base every
// other user process gets — runtime.UserProcessEnvironment layers the
// sandbox binding envs and the EXECD_ENVS file over the daemon environment
// and strips execd's own config env — then filters the merged result again
// (the env-file overlay is applied after UserProcessEnvironment's own
// filtering and could reintroduce reserved credential entries) and overlays
// the registration's env entries.
func childEnviron(extra map[string]string) []string {
	return append(filterBlacklistedEnv(runtime.UserProcessEnvironment()), envSlice(extra)...)
}

// filterBlacklistedEnv drops execd config/credential entries from a merged
// environment slice.
func filterBlacklistedEnv(env []string) []string {
	blocked := make(map[string]struct{})
	for _, k := range isolation.ExecdConfigEnvBlacklist() {
		blocked[strings.ToUpper(k)] = struct{}{}
	}
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, bad := blocked[strings.ToUpper(k)]; !bad {
			out = append(out, kv)
		}
	}
	return out
}

// resolveChildCommand resolves a command name against the PATH the child
// will actually run with. Paths containing a separator and names not found
// in the child PATH are returned unchanged (the latter keeps exec's natural
// not-found error at spawn time).
func resolveChildCommand(command string, env []string, dir string) string {
	if command == "" || strings.ContainsRune(command, '/') || strings.ContainsRune(command, os.PathSeparator) {
		return command
	}
	childPath := ""
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PATH") {
			childPath = v
			break
		}
	}
	if childPath == "" {
		return command
	}
	for _, d := range filepath.SplitList(childPath) {
		if d == "" {
			d = "."
		}
		if dir != "" && !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		candidate := filepath.Join(d, command)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return command
}

// envSlice flattens an env map for exec.Cmd.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
