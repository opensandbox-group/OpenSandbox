# Contributing to OpenSandbox

Thank you for your interest in contributing to OpenSandbox! This guide will help you get started with contributing to the project, whether you're fixing bugs, adding features, improving documentation, or helping in other ways.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Environment Setup](#development-environment-setup)
- [Project Structure](#project-structure)
- [Development Workflow](#development-workflow)
- [Coding Standards](#coding-standards)
- [Testing Guidelines](#testing-guidelines)
- [Submitting Contributions](#submitting-contributions)
- [Communication Channels](#communication-channels)

## Code of Conduct

OpenSandbox adheres to a [Code of Conduct](CODE_OF_CONDUCT.md) that we expect all contributors to follow. Please read it before contributing to ensure a welcoming and inclusive environment for everyone.

## Getting Started

### Ways to Contribute

There are many ways to contribute to OpenSandbox:

- **Report Bugs**: Submit detailed bug reports through [GitHub Issues](https://github.com/alibaba/OpenSandbox/issues)
- **Suggest Features**: Propose new features or improvements
- **Write Code**: Fix bugs, implement features, or improve performance
- **Improve Documentation**: Enhance README files, write tutorials, or fix typos
- **Write Tests**: Add test coverage or improve existing tests
- **Review Pull Requests**: Help review and test others' contributions
- **Answer Questions**: Help other users in GitHub Discussions or Issues

### Before You Start

1. **Search Existing Issues**: Check if your bug report or feature request already exists
2. **Check Roadmap**: Review the project roadmap to see if your idea aligns with project goals
3. **Discuss Major Changes**: For significant changes, open an issue first or submit an [OSEP](oseps/README.md) to discuss your approach
4. **Review Architecture**: Read [docs/architecture.md](docs/architecture.md) to understand the system design

## Development Environment Setup

### Prerequisites

Different components have different requirements:

#### For Server (Python)

- **Python 3.10+**
- **uv** - Python package manager ([installation guide](https://github.com/astral-sh/uv))
- **Docker** - For running sandboxes locally

#### For execd (Go)

- **Go 1.24+**
- **Make** - Build automation (optional)
- **Docker** - For building container images

#### For SDKs

- **Python SDK**: Python 3.10+, uv
- **Java/Kotlin SDK**: JDK 17+, Gradle

### Quick Setup

#### Server Development

```bash
# Navigate to server directory
cd server

# Install dependencies
uv sync

# Copy example configuration from the source tree
cp server/opensandbox_server/examples/example.config.toml ~/.sandbox.toml

# Edit configuration for development
# Set [log] level = "DEBUG" and [server] api_key
nano ~/.sandbox.toml

# Run server
uv run python -m opensandbox_server.main
```

See [server/DEVELOPMENT.md](server/DEVELOPMENT.md) for detailed server development guide.

#### execd Development

```bash
# Navigate to execd directory
cd components/execd

# Download dependencies
go mod download

# Build execd
go build -o bin/execd .

# Run execd (requires Jupyter Server)
./bin/execd --jupyter-host=http://localhost:8888 --port=44772
```

See [components/execd/DEVELOPMENT.md](components/execd/DEVELOPMENT.md) for detailed execd development guide.

#### SDK Development

**Python SDK:**

```bash
cd sdks/sandbox/python
uv sync
uv run pytest
```

**Java/Kotlin SDK:**

```bash
cd sdks/sandbox/kotlin
./gradlew build
./gradlew test
```

## Project Structure

```
OpenSandbox/
├── sdks/                     # Multi-language SDKs
│   ├── code-interpreter/     # Code Interpreter SDK (Python, Kotlin)
│   └── sandbox/              # Sandbox base SDK (Python, Kotlin)
├── specs/                    # OpenAPI specifications
│   ├── execd-api.yaml        # Execution API spec
│   └── sandbox-lifecycle.yml # Lifecycle API spec
├── server/                   # Sandbox server (Python/FastAPI)
├── components/
│   └── execd/                # Execution daemon (Go/Beego)
├── sandboxes/                # Sandbox implementations
│   └── code-interpreter/     # Code Interpreter sandbox
├── examples/                 # Example integrations
├── docs/                     # Documentation
├── tests/                    # Cross-component tests
│   └── e2e/                  # End-to-end tests
└── scripts/                  # Build and utility scripts
```

## Development Workflow

### Enhancement Proposals (OSEP)

For major features, architectural changes, or modifications to the core API/security model, we follow the **OSEP (OpenSandbox Enhancement Proposals)** process.

Please read the [OSEP README](oseps/README.md) to understand when an OSEP is required and how to submit one. Small bug fixes and minor improvements do not require an OSEP.

### Branching Strategy

- **main**: Stable production branch
- **feature/[name]**: New features
- **fix/[name]**: Bug fixes
- **docs/[name]**: Documentation updates
- **refactor/[name]**: Code refactoring
- **test/[name]**: Test additions or improvements

### Creating a Feature Branch

```bash
# Update main branch
git checkout main
git pull origin main

# Create feature branch
git checkout -b feature/my-awesome-feature

# Make your changes
# ...

# Commit your changes
git add .
git commit -m "feat: add my awesome feature"

# Push to your fork
git push origin feature/my-awesome-feature
```

### Commit Message Format

We follow [Conventional Commits](https://www.conventionalcommits.org/) specification:

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

**Types:**

- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `style`: Code style changes (formatting, no logic change)
- `refactor`: Code refactoring
- `test`: Adding or updating tests
- `chore`: Build process, dependencies, or tooling changes
- `perf`: Performance improvements
- `ci`: CI/CD changes

**Examples:**

```
feat(server): add Kubernetes runtime support
fix(execd): resolve memory leak in session cleanup
docs(sdk): add Python SDK usage examples
test(server): add integration tests for Docker runtime
refactor(sdk): simplify filesystem API
```

### Making Changes

1. **Write Clean Code**: Follow project coding standards (see below)
2. **Add Tests**: Ensure your changes are covered by tests
3. **Update Documentation**: Update relevant documentation files
4. **Test Locally**: Run all tests and ensure they pass
5. **Check Linting**: Run linters and fix any issues

## Coding Standards

Contributions are required to generally comply with the coding standards for
the language and component they touch. Generated files are excluded where the
local tool configuration excludes them; update the source specification or
generator and regenerate those files instead of hand-editing generated output.

### Required Style Guides

| Area | Primary language | Required style guide |
| --- | --- | --- |
| Server, CLI, Python SDKs, Python tests | Python | [PEP 8](https://peps.python.org/pep-0008/) plus Google-style docstrings for public APIs |
| Go components, Kubernetes controller, Go SDK | Go | [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) |
| JavaScript/TypeScript SDKs | JavaScript/TypeScript | [TypeScript ESLint recommended and stylistic rule sets](https://typescript-eslint.io/users/configs/) |
| Kotlin SDKs | Kotlin | [Kotlin Coding Conventions](https://kotlinlang.org/docs/coding-conventions.html) |
| C# SDKs | C# | [Microsoft C# coding conventions](https://learn.microsoft.com/dotnet/csharp/fundamentals/coding-style/coding-conventions) |

### Automated Enforcement

| Area | Tooling and CI entry point |
| --- | --- |
| Server | `server/pyproject.toml`; `.github/workflows/server-test.yml` runs `uv run ruff check` |
| CLI and Python SDKs | package `pyproject.toml` files; `.github/workflows/sdk-tests.yml` runs `uv run ruff check` and `uv run pyright` |
| Go components and Kubernetes | `gofmt`, `go vet`, and `golangci-lint` where configured; component workflows run format, lint, build, and test checks |
| Go SDK | `.github/workflows/sdk-tests.yml` runs `gofmt`, `go vet`, and tests |
| JavaScript/TypeScript SDKs | `sdks/eslint.base.mjs` and package `eslint.config.mjs`; `.github/workflows/sdk-tests.yml` runs `pnpm run lint` and `pnpm run typecheck` |
| Kotlin SDKs | Gradle Spotless with ktlint; `.github/workflows/sdk-tests.yml` runs `./gradlew spotlessCheck ...` |
| C# SDKs | `.editorconfig`, `Directory.Build.props`, and .NET analyzers; `.github/workflows/sdk-tests.yml` runs `dotnet build ... /warnaserror` |

### Python (Server, CLI, Python SDKs)

- **Style Guide**: Follow [PEP 8](https://peps.python.org/pep-0008/)
- **Linter/Formatter**: Use `ruff` for linting and formatting
- **Type Hints**: Always use type hints for function signatures
- **Docstrings**: Use Google-style docstrings for public APIs

```python
def create_sandbox(
    image: ImageSpec,
    timeout: timedelta,
    entrypoint: Optional[List[str]] = None
) -> Sandbox:
    """Create a new sandbox instance.

    Args:
        image: Container image specification
        timeout: Sandbox timeout duration
        entrypoint: Optional custom entrypoint command

    Returns:
        Created sandbox instance

    Raises:
        ValueError: If image or timeout is invalid
    """
    # Implementation
```

**Running Linter:**

```bash
cd server
uv run ruff check
uv run ruff format opensandbox_server tests
```

### Go (components, Kubernetes, Go SDK)

- **Style Guide**: Follow [Effective Go](https://go.dev/doc/effective_go)
- **Formatter**: Use `gofmt` for formatting
- **Imports**: Organize in three groups (stdlib, third-party, internal)
- **Error Handling**: Always handle errors explicitly

```go
// Good
result, err := someOperation()
if err != nil {
    logs.Error("operation failed: %v", err)
    return fmt.Errorf("failed to do something: %w", err)
}

// Bad - silent failure
result, _ := someOperation()
```

**Running Formatter:**

```bash
cd components/execd
gofmt -w .
# Or
make fmt
```

### JavaScript/TypeScript (SDKs)

- **Style Guide**: Follow the TypeScript ESLint recommended and stylistic rule sets configured in `sdks/eslint.base.mjs`
- **Linter**: Use `eslint` for JavaScript/TypeScript linting
- **Type Checking**: Run `tsc` with the package `tsconfig.json`

**Running Checks:**

```bash
cd sdks
pnpm run lint:js
pnpm run typecheck:js
```

### Java/Kotlin (Java/Kotlin SDKs)

- **Style Guide**: Follow [Kotlin Coding Conventions](https://kotlinlang.org/docs/coding-conventions.html)
- **Formatter**: Use `ktlint`
- **Null Safety**: Use Kotlin's null safety features

```kotlin
suspend fun createSandbox(
    image: ImageSpec,
    timeout: Duration,
    entrypoint: List<String>? = null
): Sandbox {
    // Implementation
}
```

### C# (C# SDKs)

- **Style Guide**: Follow [Microsoft C# coding conventions](https://learn.microsoft.com/dotnet/csharp/fundamentals/coding-style/coding-conventions)
- **Formatting**: Follow the SDK `.editorconfig` files
- **Analyzers**: Keep .NET analyzers enabled and treat build warnings as errors in CI

**Running Checks:**

```bash
cd sdks/sandbox/csharp
dotnet build OpenSandbox.sln --configuration Release /warnaserror

cd ../../code-interpreter/csharp
dotnet build OpenSandbox.CodeInterpreter.sln --configuration Release /warnaserror
```

### General Guidelines

- **Naming Conventions**:
  - Functions/Methods: `snake_case` (Python), `camelCase` (Go, Kotlin)
  - Classes: `PascalCase` (all languages)
  - Constants: `UPPER_SNAKE_CASE` (all languages)
  - Private members: `_leading_underscore` (Python), `unexported` (Go)

- **Comments**: Write clear, concise comments explaining "why", not "what"
- **Error Messages**: Provide actionable error messages with context
- **Logging**: Use appropriate log levels (DEBUG, INFO, WARNING, ERROR)

## Build System Standards

OpenSandbox produces native Go binaries for `components/execd`, `components/ingress`,
`components/egress`, `kubernetes`, and `sdks/sandbox/go`. These build systems must
preserve caller-provided build flags and only append project-required flags.

### Build Variables

- Go builds pass caller-provided `GOFLAGS` to `go build` and append project flags such as `-trimpath` and `-buildvcs=false`.
- Go linker builds pass caller-provided `LDFLAGS` to `go build -ldflags` and append project metadata flags.
- When CGO is enabled, the Go toolchain honors `CC`, `CXX`, `CGO_CFLAGS`, `CGO_CXXFLAGS`, and `CGO_LDFLAGS`. Docker-based builds also accept `CFLAGS` and `CXXFLAGS` as fallbacks for `CGO_CFLAGS` and `CGO_CXXFLAGS`.
- Docker build scripts forward these variables as build arguments when they are present in the environment.

### Debug Information

Default project builds must not strip debug information. Do not add `strip`,
`install -s`, or Go linker flags such as `-s -w` to the default build path.
Distribution-specific packaging may strip binaries only outside the default
developer and CI build path.

### Build Dependency Graph

Use package-aware build tools instead of recursive independent builds:

- Go packages are built through `go build ./...` or explicit Go package entry points.
- Kotlin projects use Gradle task dependencies.
- JavaScript and TypeScript SDKs use pnpm workspace dependencies.
- C# SDKs use solution/project references through `dotnet build`.

Subdirectory-specific Make targets may delegate to these tools, but they must not
replace the build tool's dependency graph with independent recursive directory
builds where cross-directory dependencies exist.

### Repeatable Builds

Native Go binary builds include `-trimpath`, `-buildvcs=false`, and `-ldflags`
with `-buildid= -B none` so that source paths, VCS metadata, and build IDs or
Mach-O UUIDs do not make binaries differ. For repeatable release metadata, set `SOURCE_DATE_EPOCH`
or set `BUILD_TIME`, `VERSION`, and `GIT_COMMIT` explicitly before invoking
Makefile or Docker build scripts. When `SOURCE_DATE_EPOCH` is set and
`BUILD_TIME` is unset, build scripts derive `BUILD_TIME` from
`SOURCE_DATE_EPOCH`.

## Testing Guidelines

### Test Coverage Requirements

- **Core Packages**: Aim for >80% coverage
- **API Layer**: Aim for >70% coverage
- **Utilities**: Aim for >90% coverage

### Writing Tests

#### Python Tests (pytest)

```python
import pytest
from opensandbox import Sandbox

@pytest.mark.asyncio
async def test_create_sandbox():
    """Test sandbox creation with valid parameters."""
    sandbox = await Sandbox.create(
        image="python:3.11",
        timeout=timedelta(minutes=5)
    )
    assert sandbox.id is not None
    assert sandbox.status == SandboxStatus.PENDING
    await sandbox.kill()

@pytest.mark.asyncio
async def test_invalid_timeout():
    """Test sandbox creation fails with invalid timeout."""
    with pytest.raises(ValueError):
        await Sandbox.create(
            image="python:3.11",
            timeout=timedelta(seconds=-1)
        )
```

**Running Tests:**

```bash
cd server
uv run pytest
uv run pytest --cov=src --cov-report=html
```

#### Go Tests

```go
func TestController_Execute_Python(t *testing.T) {
    ctrl := NewController("http://localhost:8888", "test-token")

    req := &ExecuteCodeRequest{
        Language: Python,
        Code:     "print('hello')",
    }

    err := ctrl.Execute(req)
    assert.NoError(t, err)
}
```

**Running Tests:**

```bash
cd components/execd
go test ./pkg/...
go test -v -cover ./pkg/...
```

#### Integration Tests

Integration tests require Docker:

```bash
# Server integration tests
cd server
uv run pytest tests/integration/

# E2E tests
cd tests/e2e/python
uv run pytest
```

### Test Best Practices

- **Test Names**: Use descriptive names that explain what is being tested
- **Arrange-Act-Assert**: Structure tests clearly
- **Isolation**: Each test should be independent
- **Mocking**: Mock external dependencies appropriately
- **Cleanup**: Always clean up resources (use fixtures, context managers)

## Submitting Contributions

### Pull Request Process

1. **Create Feature Branch**: Branch from `main`
2. **Make Changes**: Implement your feature or fix
3. **Write Tests**: Add comprehensive test coverage
4. **Update Documentation**: Update relevant docs
5. **Test Locally**: Ensure all tests pass
6. **Run Linters**: Fix any style issues
7. **Commit Changes**: Use conventional commit messages
8. **Push to Fork**: Push your branch to your fork
9. **Create Pull Request**: Submit PR with detailed description

### Pull Request Template

When creating a PR, fill out the template:

```markdown
# Summary

- What is changing and why?

# Testing

- [ ] Not run (explain why)
- [ ] Unit tests
- [ ] Integration tests
- [ ] e2e / manual verification

# Breaking Changes

- [ ] None
- [ ] Yes (describe impact and migration path)

# Checklist

- [ ] Linked Issue or clearly described motivation
- [ ] Added/updated docs (if needed)
- [ ] Added/updated tests (if needed)
- [ ] Security impact considered
- [ ] Backward compatibility considered
```

### Pull Request Guidelines

**Do:**

- Keep PRs focused and reasonably sized (< 500 lines if possible)
- Write clear PR descriptions with motivation and context
- Link related issues
- Respond to review comments promptly
- Update your PR based on feedback
- Ensure CI passes before requesting review

**Don't:**

- Mix multiple unrelated changes in one PR
- Submit PRs with failing tests
- Ignore code review feedback
- Force push after reviews have started (unless necessary)
- Include commented-out code or debug statements

### Code Review Process

1. **Automated Checks**: CI runs tests, linters, and security scans
2. **Maintainer Review**: A maintainer reviews your code
3. **Feedback Loop**: Address review comments
4. **Approval**: Once approved, a maintainer will merge your PR
5. **Cleanup**: Delete your feature branch after merge

## Communication Channels

### GitHub Issues

Use GitHub Issues for:

- Bug reports
- Feature requests
- Documentation improvements
- Questions about implementation

**Bug Report Template:**

```markdown
**Description**
A clear description of the bug.

**To Reproduce**
Steps to reproduce the behavior:

1. Create sandbox with...
2. Execute command...
3. See error

**Expected Behavior**
What you expected to happen.

**Environment**

- OpenSandbox version:
- Runtime (Docker/K8s):
- OS:
- Python/Go version:

**Additional Context**
Logs, screenshots, or other relevant information.
```

### GitHub Discussions

Use GitHub Discussions for:

- General questions
- Design discussions
- Brainstorming ideas
- Community help

### Getting Help

- **Issues**: Technical problems or bugs
- **Discussions**: Questions and community support
- **Email**: For security issues, email conduct@opensandbox.io

## Additional Resources

### Documentation

- [Architecture Overview](docs/architecture.md)
- [Server Development Guide](server/DEVELOPMENT.md)
- [execd Development Guide](components/execd/DEVELOPMENT.md)
- [OpenAPI Specifications](specs/README.md)
- [Python SDK Documentation](sdks/sandbox/python/README.md)
- [Java/Kotlin SDK Documentation](sdks/sandbox/kotlin/README.md)

### Examples

Browse [examples/](examples/) for real-world usage patterns:

- Code Interpreter integration
- AI Coding Agent integrations (Claude Code, Gemini CLI, etc.)
- Browser automation (Chrome, Playwright)
- Remote development (VS Code, Desktop)

### External Resources

- [FastAPI Documentation](https://fastapi.tiangolo.com/)
- [Beego Documentation](https://beego.wiki/)
- [Jupyter Protocol](https://jupyter-client.readthedocs.io/en/stable/messaging.html)
- [OpenAPI Specification](https://swagger.io/specification/)
- [Docker API](https://docs.docker.com/engine/api/)

## Acknowledgments

Thank you for contributing to OpenSandbox! Your contributions help make this project better for everyone in the AI and developer tools community.

If you have suggestions for improving this contributing guide, please open an issue or submit a pull request.

## License

By contributing to OpenSandbox, you agree that your contributions will be licensed under the [Apache 2.0 License](LICENSE).
