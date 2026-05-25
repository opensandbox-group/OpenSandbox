# Development Guide - execd

This comprehensive guide explains how to work on `execd` as a contributor or maintainer. It covers environment setup,
development workflows, testing strategies, architectural patterns, and subsystem-specific implementation details.

## Table of Contents

- [Getting Started](#getting-started)
- [Project Structure](#project-structure)
- [Coding Standards](#coding-standards)
- [Testing Strategy](#testing-strategy)
- [Subsystem Guides](#subsystem-guides)
- [Common Development Tasks](#common-development-tasks)
- [Debugging Techniques](#debugging-techniques)
- [Performance Optimization](#performance-optimization)
- [Contributing Guidelines](#contributing-guidelines)
- [Additional Resources](#additional-resources)

## Getting Started

### Prerequisites

#### Required Tools

- **Go 1.24+** - Match the version declared in `go.mod`
- **Git** - Version control
- **Make** - Build automation (optional but recommended)

#### Optional but Recommended

- **golangci-lint** - For comprehensive linting
- **Docker/Podman** - For containerized testing and deployment
- **Jupyter Server** - Required for integration tests with real kernels
- **VS Code/GoLand** - IDE with Go support

### Initial Setup

```bash
# Clone the repository
git clone https://github.com/alibaba/OpenSandbox.git
cd OpenSandbox/components/execd

# Download dependencies
go mod download

# Verify setup
go build -o bin/execd .
```

## Project Structure

### Project Structure Deep Dive

```
execd/
├── main.go                 # Application entry point
├── go.mod                  # Go module definition
├── Makefile               # Build automation
├── Dockerfile             # Container image definition
│
├── pkg/                   # Public packages
│   ├── flag/              # CLI flag parsing
│   ├── web/               # HTTP layer
│   │   ├── router.go      # Route registration
│   │   ├── controller/    # Request handlers
│   │   └── model/         # API models
│   ├── runtime/           # Execution engine
│   │   ├── ctrl.go        # Main controller
│   │   ├── jupyter.go     # Jupyter execution
│   │   └── command.go     # Shell command execution
│   ├── jupyter/           # Jupyter client
│   │   ├── client.go      # HTTP/WebSocket client
│   │   ├── session/       # Session management
│   │   └── execute/       # Execution protocol
│   └── util/              # Utilities
│
└── tests/                # Integration test scripts
```

### Key Design Patterns

#### 1. Controller Pattern (pkg/web/controller)

Controllers are thin HTTP handlers that parse requests, validate, delegate to runtime, and stream responses via SSE.

#### 2. Runtime Controller Pattern (pkg/runtime)

The runtime controller dispatches requests to appropriate executors (Jupyter, Command, SQL) and manages session
lifecycle.

#### 3. Hook Pattern for Streaming

Execution results are streamed via hooks, allowing controllers to transform runtime events into SSE events without tight
coupling.

## Coding Standards

### Go Conventions

#### Formatting

**Always use `gofmt`** before committing:

```bash
gofmt -w .
# or
make fmt
```

#### Import Organization

Three groups separated by blank lines:

```go
import (
    // Standard library
    "context"
    "fmt"
    "net/http"

    // Third-party
    "github.com/gin-gonic/gin"

    // Internal
    "github.com/alibaba/opensandbox/execd/pkg/log"
    "github.com/alibaba/opensandbox/execd/pkg/runtime"
)
```

#### Error Handling

Always handle errors explicitly:

```go
// Good
result, err := someOperation()
if err != nil {
    log.Error("operation failed: %v", err)
    return fmt.Errorf("failed to do something: %w", err)
}

// Bad - silent failure
result, _ := someOperation()
```

#### Logging

Use execd's package logger, which wraps the shared structured logger used by the Gin HTTP layer and runtime code:

```go
log.Info("starting execution: sessionID=%s", sessionID)
log.Warning("session busy: sessionID=%s", sessionID)
log.Error("execution failed: error=%v", err)
log.Debug("received event: type=%s", eventType)
```

### Concurrency Best Practices

#### Use safego for goroutines

Always use `safego.Go` to prevent panics:

```go
import "github.com/alibaba/opensandbox/internal/safego"

safego.Go(func() {
    processInBackground()
})
```

#### Context Propagation

Always respect context cancellation:

```go
func (c *Controller) runCommand(ctx context.Context, req *ExecuteCodeRequest) error {
    cmd := exec.CommandContext(ctx, "bash", "-c", req.Code)

    go func() {
        <-ctx.Done()
        if cmd.Process != nil {
            cmd.Process.Kill()
        }
    }()

    return cmd.Run()
}
```

## Testing Strategy

### Unit Tests

Located in `*_test.go` files alongside source code.

**Example:**

```go
func TestController_Execute_Python(t *testing.T) {
    ctrl := NewController("http://jupyter:8888", "test-token")

    req := &ExecuteCodeRequest{
        Language: Python,
        Code:     "print('hello')",
    }

    err := ctrl.Execute(req)
    assert.NoError(t, err)
}
```

**Running Unit Tests:**

```bash
go test ./pkg/...
# with coverage
go test -v -cover ./pkg/...
```

### Integration Tests

Located in `*_integration_test.go`, require real dependencies.

**Running Integration Tests:**

```bash
export JUPYTER_URL=http://localhost:8888
export JUPYTER_TOKEN=your-token
go test -v ./pkg/jupyter/...
```

### Test Coverage

Check coverage:

```bash
go test -coverprofile=coverage.out ./pkg/...
go tool cover -html=coverage.out -o coverage.html
```

**Coverage Goals:**

- Core packages (`pkg/runtime`, `pkg/jupyter`): > 80%
- Controllers (`pkg/web/controller`): > 70%
- Utilities (`pkg/util`): > 90%

## Subsystem Guides

### Working with Jupyter Integration

#### Architecture

```
pkg/jupyter/
├── client.go          # Main client
├── transport.go       # Connection handling
├── session/           # Session lifecycle
├── execute/           # Execution protocol
└── auth/              # Authentication
```

#### Adding New Kernel Support

1. Define language in `pkg/runtime/language.go`:

```go
const Ruby Language = "ruby"
```

2. Map to kernel in `pkg/runtime/jupyter.go`

3. Test with real kernel:

```bash
# Install Ruby kernel
gem install iruby
iruby register --force

# Run test
export JUPYTER_URL=http://localhost:8888
go test -v ./pkg/jupyter/integration_test.go
```

#### Debugging Jupyter Communication

Run debug integration test:

```bash
go test -v ./pkg/jupyter/debug_integration_test.go
```

This dumps complete HTTP request/response pairs.

### Working with Command Execution

#### Key Implementation Details

**Process Group Management:**

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setpgid: true,  // Create new process group
}
```

This allows signal forwarding to all child processes:

```go
syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
```

**Signal Forwarding:**

```go
signals := make(chan os.Signal, 1)
signal.Notify(signals)

go func() {
    for sig := range signals {
        if sig != syscall.SIGCHLD && sig != syscall.SIGURG {
            syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
        }
    }
}()
```

**Stdout/Stderr Streaming:**

Commands write to temporary log files, which are tailed and streamed to hooks.

## Common Development Tasks

### Adding a New API Endpoint

1. **Define model** in `pkg/web/model/`:

```go
type NewFeatureRequest struct {
    Param1 string `json:"param1" validate:"required"`
    Param2 int    `json:"param2"`
}
```

2. **Add controller method** in `pkg/web/controller/`:

```go
func (c *MyController) NewFeature() {
    var req model.NewFeatureRequest
    json.Unmarshal(c.Ctx.Input.RequestBody, &req)

    // Business logic
    result := processNewFeature(req)

    c.Data["json"] = result
    c.ServeJSON()
}
```

3. **Register route** in `pkg/web/router.go`:

```go
myNamespace := web.NewNamespace("/my-feature",
    web.NSRouter("", &controller.MyController{}, "post:NewFeature"),
)
web.AddNamespace(myNamespace)
```

### Adding Configuration Flag

1. **Declare in `pkg/flag/flags.go`:**

```go
var NewFeatureTimeout time.Duration
```

2. **Parse in `pkg/flag/parser.go`:**

```go
func InitFlags() {
    flag.DurationVar(&NewFeatureTimeout, "new-feature-timeout", 30*time.Second, "Description")

    // Parse environment variable
    if env := os.Getenv("NEW_FEATURE_TIMEOUT"); env != "" {
        if d, err := time.ParseDuration(env); err == nil {
            NewFeatureTimeout = d
        }
    }

    flag.Parse()
}
```

3. **Update README** with new flag documentation

## Debugging Techniques

### Local Debugging with Delve

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Start debugging
dlv debug . -- \
  --jupyter-host=http://localhost:8888 \
  --jupyter-token=test

# Set breakpoint
(dlv) break pkg/runtime/ctrl.go:57
(dlv) continue
```

### Debugging SSE Streams

**Test with curl:**

```bash
curl -N -H "x-access-token: dev" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"test\")"}' \
  http://localhost:44772/code
```

The `-N` flag disables buffering for real-time events.

**Debug in browser:**

```javascript
const eventSource = new EventSource('/code');

eventSource.addEventListener('stdout', (e) => {
    console.log('stdout:', e.data);
});

eventSource.addEventListener('error', (e) => {
    console.error('error:', e.data);
});
```

### Performance Profiling

**CPU Profile:**

```bash
# Add to main.go
import _ "net/http/pprof"

go func() {
    http.ListenAndServe("localhost:6060", nil)
}()

# Collect profile
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

**Memory Profile:**

```bash
go tool pprof http://localhost:6060/debug/pprof/heap
```

**Goroutine Inspection:**

```bash
curl http://localhost:6060/debug/pprof/goroutine?debug=2
```

## Performance Optimization

### Optimization Guidelines

1. **Profile before optimizing** - Use pprof to identify bottlenecks
2. **Benchmark changes** - Measure impact of optimizations
3. **Use `sync.Pool`** for frequently allocated objects
4. **Minimize allocations** in hot paths
5. **Buffer channels** appropriately

### Example: Optimizing SSE Writer

**Before:**

```go
func writeEvent(w http.ResponseWriter, event, data string) {
    fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
    w.(http.Flusher).Flush()
}
```

**After:**

```go
var bufPool = sync.Pool{
    New: func() interface{} { return new(bytes.Buffer) },
}

func writeEvent(w http.ResponseWriter, event, data string) {
    buf := bufPool.Get().(*bytes.Buffer)
    buf.Reset()
    defer bufPool.Put(buf)

    buf.WriteString("event: ")
    buf.WriteString(event)
    buf.WriteString("\ndata: ")
    buf.WriteString(data)
    buf.WriteString("\n\n")

    w.Write(buf.Bytes())
    w.(http.Flusher).Flush()
}
```

**Benchmark:**

```go
func BenchmarkWriteEvent(b *testing.B) {
    w := httptest.NewRecorder()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        writeEvent(w, "test", "data")
    }
}
```

## Contributing Guidelines

### Pull Request Process

1. **Fork and clone** the repository
2. **Create feature branch** from `main`
3. **Implement changes** following coding standards
4. **Add tests** for new functionality
5. **Run all tests** and ensure they pass
6. **Update documentation** as needed
7. **Submit PR** with clear description

### Code Review Standards

Reviewers check for:

- [ ] Correctness and functionality
- [ ] Test coverage
- [ ] Code style and formatting
- [ ] Documentation completeness
- [ ] Performance implications
- [ ] Security considerations
- [ ] Error handling
- [ ] Backwards compatibility

### Release Checklist

Before releasing:

- [ ] All tests pass (unit, integration, e2e)
- [ ] Documentation updated (README, DEVELOPMENT, API docs)
- [ ] CHANGELOG updated with changes
- [ ] Version bumped appropriately (semver)
- [ ] Dependencies reviewed and updated
- [ ] Security scan passed
- [ ] Performance benchmarks run
- [ ] Docker image built and tested

## Additional Resources

### Useful Commands

```bash
# Format all Go files
make fmt

# Run linter
make golint

# Run all tests
make test

# Build binary
make build
```

### External Documentation

- [Gin Documentation](https://gin-gonic.com/docs/)
- [Jupyter Kernel Protocol](https://jupyter-client.readthedocs.io/en/stable/messaging.html)
- [Go Best Practices](https://golang.org/doc/effective_go)
- [Server-Sent Events Spec](https://html.spec.whatwg.org/multipage/server-sent-events.html)

### Getting Help

- **Issues**: Report bugs or request features on GitHub Issues
- **Discussions**: Ask questions in GitHub Discussions
- **Chat**: Join the OpenSandbox community chat
- **Documentation**: Check the wiki for detailed guides

---

**Happy hacking!** Feel free to augment this guide with tips you discover along the way. For questions or suggestions,
open an issue or discussion on GitHub.
