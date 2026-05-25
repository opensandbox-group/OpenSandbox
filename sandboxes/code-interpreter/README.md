# OpenSandbox Code Interpreter Environment

English | [中文](README_zh.md)

This directory contains the Docker build files for the Code Interpreter sandbox. The image is based on `Ubuntu 24.04`
and comes pre-installed with multiple mainstream programming languages and their multi-version environments, designed to
provide an out-of-the-box multi-language code execution environment.

## Features

- **Multi-Language Support**: Pre-installed Python, Java, Node.js, and Go with multiple versions
- **Version Switching**: Easy runtime version switching without rebuilding
- **Jupyter Integration**: Built-in Jupyter Notebook with multi-language kernels
- **Multi-Architecture**: Supports both amd64 and arm64 architectures
- **clone3-workaround (amd64)**: The image installs [AkihiroSuda/clone3-workaround](https://github.com/AkihiroSuda/clone3-workaround) v1.0.0 as `/usr/local/bin/clone3-workaround` on **linux/amd64** only (upstream ships no arm64 binary), plus **`libseccomp2`** because the upstream binary is dynamically linked to `libseccomp`. Use it to wrap commands on very old Docker/containerd hosts, e.g. `clone3-workaround apt-get update`.
- **Production Ready**: Optimized for containerized execution environments

## Supported Languages & Versions

The image comes pre-installed with the following languages and versions:

| Language    | Supported Versions            | Installation Path      | Notes                                    |
|:------------|:------------------------------|:-----------------------|:-----------------------------------------|
| **Python**  | 3.10, 3.11, 3.12, 3.13, 3.14* | `/opt/python/versions` | Installed via `uv`; 3.14 is experimental |
| **Java**    | 8, 11, 17, 21                 | `/usr/lib/jvm`         | OpenJDK; includes Maven 3.9.2            |
| **Node.js** | v18, v20, v22                 | `/opt/node`            | Official Linux binaries                  |
| **Go**      | 1.23, 1.24, 1.25              | `/opt/go`              | Official Linux binaries                  |

*> Note: Version numbers may be updated to the latest patch versions at build time.*

## Quick Start

### 1. Build the Image

Since multi-architecture (amd64/arm64) is supported, it's recommended to use Docker Buildx:

```bash
# Navigate to the directory
cd sandboxes/code-interpreter

# Build local image
docker build -t opensandbox/code-interpreter:latest .

# For multi-architecture builds (requires Docker Buildx)
docker buildx build --platform linux/amd64,linux/arm64 \
  -t opensandbox/code-interpreter:latest .
```

### 2. Run the Container

**With Custom Version Selection:**

```bash
docker run -it --rm \
  -e PYTHON_VERSION=3.11 \
  -e JAVA_VERSION=17 \
  -e NODE_VERSION=20 \
  -e GO_VERSION=1.24 \
  opensandbox/code-interpreter:latest
```

### `EXECD_CLONE3_COMPAT` (clone3-workaround)

If you set `EXECD_CLONE3_COMPAT` to `1`, `true`, `yes`, `on`, or `reexec` (same semantics as [execd](../../components/execd/README.md#linux-clone3-compatibility-inside-sandboxes)), the entrypoint script **re-executes itself** under `/usr/local/bin/clone3-workaround` before Jupyter and kernel setup. That binary is included on **linux/amd64** only; on **arm64** builds the script prints a warning and continues without wrapping. After a successful wrap, the script **unsets** `EXECD_CLONE3_COMPAT` in the running process tree. Use `0`, `false`, `off`, `no`, or leave unset to disable.

## Version Switching

The image includes a built-in version switching script `/opt/opensandbox/code-interpreter-env.sh`. You need to use the
`source` command to load it to modify the current shell's environment variables.

### Basic Usage

```bash
source /opt/opensandbox/code-interpreter-env.sh <language> <version>
```

### Examples

**Switch Python Version:**

```bash
# Switch to Python 3.11
source /opt/opensandbox/code-interpreter-env.sh python 3.11
python3 --version
# Output: Python 3.11.x
```

**Switch Java Version:**

```bash
# Switch to Java 8
source /opt/opensandbox/code-interpreter-env.sh java 8
java -version
```

**Switch Node.js Version:**

```bash
# Switch to Node 22
source /opt/opensandbox/code-interpreter-env.sh node 22
node -v
```

**Switch Go Version:**

```bash
# Switch to Go 1.25
source /opt/opensandbox/code-interpreter-env.sh go 1.25
go version
```

### List Available Versions

If you don't specify a version number, the script will list all available versions installed in the current image:

```bash
# List all Python versions
source /opt/opensandbox/code-interpreter-env.sh python

# List all Java versions
source /opt/opensandbox/code-interpreter-env.sh java

# List all Node.js versions
source /opt/opensandbox/code-interpreter-env.sh node

# List all Go versions
source /opt/opensandbox/code-interpreter-env.sh go
```

## Default Versions

The default version configuration when the container starts:

- **Python**: 3.14
- **Java**: 21
- **Node.js**: 22
- **Go**: 1.25

To permanently modify the default version at the Dockerfile level, adjust the `ENV PATH` settings at the bottom of the
Dockerfile.

## Jupyter Notebook Integration

### Available Kernels

The image comes with pre-configured Jupyter kernels for all supported languages:

- **Python**: ipykernel for all Python versions
- **Java**: IJava kernel
- **TypeScript/JavaScript**: tslab kernel
- **Go**: gonb kernel
- **Bash**: bash_kernel

### Starting Jupyter

```bash
/opt/opensandbox/code-interpreter.sh
```

### Environment Variables

- `JUPYTER_HOST`: Jupyter server host (default: `http://127.0.0.1:44771`)
- `JUPYTER_PORT`: Jupyter server port (default: `44771`)
- `JUPYTER_TOKEN`: Access token (default: `opensandboxcodeinterpreterjupyter`)

## Advanced Usage

### Persistent Workspace

Mount a local directory to persist your work:

```bash
docker run -it --rm \
  -v $(pwd)/workspace:/workspace \
  opensandbox/code-interpreter:latest
```

### Custom Configuration

Override Jupyter configuration:

```bash
docker run -it --rm \
  -v $(pwd)/jupyter_config.py:/root/.jupyter/jupyter_notebook_config.py \
  opensandbox/code-interpreter:latest
```

### Install Additional Packages

**Python:**

```bash
python3 -m pip install pandas numpy --break-system-packages
```

**Node.js:**

```bash
npm install -g typescript
```

**Go:**

```bash
go install github.com/user/package@latest
```

**Java:**

```bash
mvn install dependency:copy-dependencies
```

## Architecture

```
code-interpreter/
├── Dockerfile                          # Main build file
├── Dockerfile_base                     # Base build file
├── README.md                           # This file
├── README_zh.md                        # Chinese README
└── scripts/
    ├── code-interpreter-env.sh         # Version switching script
    ├── code-interpreter.sh             # Jupyter startup script
    └── jupyter_notebook_config.py      # Jupyter configuration
```

## Troubleshooting

If a specific version is not found, list available versions:

```bash
source /opt/opensandbox/code-interpreter-env.sh <language>
```

## License

This project is part of the OpenSandbox suite. See the main [LICENSE](../../LICENSE) file for details.

## Support

For issues and questions:

- GitHub Issues: [OpenSandbox Issues](https://github.com/alibaba/OpenSandbox/issues)

## Related Projects

- [OpenSandbox](../../) - Main project
- [Server](../../server/) - Server implementation
- [Execd](../../components/execd/) - Runtime execution engine
