# Development Guide - execd

## Getting Started

### Prerequisites

- **Go 1.24+** — match `go.mod`
- **Make** — build automation
- **Docker/Podman** — containerized testing (optional)
- **Jupyter Server** — required for integration tests

### Setup

```bash
cd components/execd
go mod download
make build        # → bin/execd
```

## Project Structure

```
execd/
├── main.go                 # Entry point
├── Makefile                # Build automation
├── Dockerfile              # Container image
├── pkg/
│   ├── flag/               # CLI flag parsing
│   ├── web/
│   │   ├── router.go       # Gin route registration
│   │   ├── controller/     # Request handlers
│   │   └── model/          # API request/response models
│   ├── runtime/            # Execution engine
│   │   ├── ctrl.go         # Main controller
│   │   ├── jupyter.go      # Jupyter kernel execution
│   │   ├── command.go      # Shell command execution
│   │   └── bash_session.go # Pipe-based bash sessions
│   ├── jupyter/            # Jupyter HTTP/WebSocket client
│   ├── telemetry/          # OTLP metrics
│   ├── clone3compat/       # Linux clone3 seccomp workaround
│   └── log/                # Structured logger wrapper
└── tests/                  # Integration test scripts
```

### Key Patterns

- **Controller pattern** (`pkg/web/controller`): thin Gin handlers that parse requests, validate, delegate to runtime, and stream responses via SSE.
- **Runtime controller** (`pkg/runtime`): dispatches to Jupyter, Command, or SQL executors; manages session lifecycle.
- **Hook-based streaming**: execution results flow through hooks, decoupling runtime events from SSE serialization.

## Testing

### Unit Tests

```bash
go test ./pkg/...
go test -v -cover ./pkg/...
```

### Integration Tests

Require a running Jupyter server:

```bash
export JUPYTER_URL=http://localhost:8888
export JUPYTER_TOKEN=your-token
go test -v ./pkg/jupyter/...
```

## Common Tasks

### Adding a New API Endpoint

1. Define request/response model in `pkg/web/model/`.

2. Add controller method in `pkg/web/controller/`:

```go
func (c *MyController) NewFeature() {
    var req model.NewFeatureRequest
    if err := c.Ctx.ShouldBindJSON(&req); err != nil {
        c.Ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    // ...
}
```

3. Register route in `pkg/web/router.go`:

```go
myGroup := r.Group("/my-feature")
{
    myGroup.POST("", withMyController(func(c *controller.MyController) { c.NewFeature() }))
}
```

### Adding a Configuration Flag

1. Declare variable in `pkg/flag/flags.go`.
2. In `pkg/flag/parser.go`, read env var first, then register `flag.*Var` with current value as default — flag overrides env.
3. Update `README.md` CLI Flags and Environment Variables tables.

### Debugging SSE Streams

```bash
curl -N -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"test\")"}' \
  http://localhost:44772/code
```

`-N` disables buffering for real-time events.

## Useful Commands

```bash
make fmt      # gofmt
make golint   # lint
make test     # all tests
make build    # binary → bin/execd
```
