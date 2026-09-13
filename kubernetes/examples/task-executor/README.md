# Task Executor Usage Guide

## Introduction

The `task-executor` is a lightweight component designed to run and manage short-lived tasks (processes or containers) within a Kubernetes Pod context. It acts as a local agent, receiving task specifications from a Kubernetes Controller (e.g., `BatchSandboxController`) and executing them on the node where it runs. It exposes a simple HTTP API for task creation, status inquiry, and management.

## Running the Task Executor

The `task-executor` can be started using the `cmd/task-executor/main.go` entry point. It supports various command-line flags and environment variables for configuration.

**Basic Startup:**

```bash
/path/to/cmd/task-executor/main --data-dir=/var/lib/sandbox/tasks --listen-addr=0.0.0.0:5758
```

**Key Configuration Parameters:**

| Flag / Environment Variable | Description                                                                                                                                                                                                                                                                                              | Default Value                 |
| :-------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :---------------------------- |
| `--data-dir` (DATA_DIR)     | Directory for persisting task state and logs.                                                                                                                                                                                                                                                            | `/var/lib/sandbox/tasks`      |
| `--listen-addr` (LISTEN_ADDR)| Address and port for the HTTP API server.                                                                                                                                                                                                                                                                | `0.0.0.0:5758`                |
| `--enable-sidecar-mode` (ENABLE_SIDECAR_MODE) | If `true`, enables sidecar mode execution, where tasks are run within the PID namespace of a specified main container. Requires `nsenter` and appropriate privileges.                                                                                                                                                            | `false`                       |
| `--main-container-name` (MAIN_CONTAINER_NAME)| When `enable-sidecar-mode` is `true`, specifies the name of the main container whose PID namespace should be used.                                                                                                                                                                       | `main`                        |
| `--cri-socket` (CRI_SOCKET) | Path to the CRI socket for container runner mode.                                                                                                                                                                                                                                                                                      | `/var/run/containerd/containerd.sock` |
| `--log-dir`                 | Directory for task-executor log files.                                                                                                                                                                                                                                                                                                 | `logs`                        |
| `--log-max-size`            | Maximum log file size in MB before rotation.                                                                                                                                                                                                                                                                                           | `100`                         |
| `--log-max-backups`         | Maximum number of rotated log backup files.                                                                                                                                                                                                                                                                                            | `10`                          |
| `--log-max-age`             | Maximum number of days to keep rotated log files.                                                                                                                                                                                                                                                                                      | `7`                           |

## HTTP API Endpoints

The `task-executor` exposes a RESTful HTTP API. All API calls expect JSON request bodies (where applicable) and return JSON responses.

### 1. `POST /tasks` - Create a new task

Creates and starts a single task.

*   **Method:** `POST`
*   **Path:** `/tasks`
*   **Request Body (application/json):** An object representing the desired task.

    ```json
    {
      "name": "my-first-task",
      "spec": {
        "process": {
          "command": ["sh", "-c"],
          "args": ["echo 'Hello from my task!' && sleep 5 && echo 'Task finished.'"]
        }
      }
    }
    ```

*   **Response Body (application/json):** The created task object with its initial status.

    ```json
    {
      "name": "my-first-task",
      "spec": {
        "process": {
          "command": ["sh", "-c"],
          "args": ["echo 'Hello from my task!' && sleep 5 && echo 'Task finished.'"]
        }
      },
      "status": {
        "state": {
          "waiting": {
            "reason": "Initialized"
          }
        }
      }
    }
    ```

**Example (using `curl`):**

```bash
curl -X POST -H "Content-Type: application/json" -d '{
  "name": "my-first-task",
  "spec": {
    "process": {
      "command": ["sh", "-c"],
      "args": ["echo \"Hello from my task!\" && sleep 5 && echo \"Task finished.\""]
    }
  }
}' http://localhost:5758/tasks
```

### 2. `GET /tasks/{id}` - Get task status

Retrieves the current status of a specific task by its name.

*   **Method:** `GET`
*   **Path:** `/tasks/{taskName}`
*   **Response Body (application/json):** The task object, including its current status.

    ```json
    {
      "name": "my-first-task",
      "spec": {
        "process": {
          "command": ["sh", "-c"],
          "args": ["echo 'Hello from my task!' && sleep 5 && echo 'Task finished.'"]
        }
      },
      "status": {
        "state": {
          "running": {
            "startedAt": "2025-12-17T10:00:00Z"
          }
        }
      }
    }
    ```

**Example (using `curl`):**

```bash
curl http://localhost:5758/tasks/my-first-task
```

### 3. `DELETE /tasks/{id}` - Delete a task

Marks a task for deletion. The `task-executor` will attempt to gracefully stop the task and then remove its state.

*   **Method:** `DELETE`
*   **Path:** `/tasks/{taskName}`
*   **Response:** `204 No Content` on successful marking for deletion.

**Example (using `curl`):**

```bash
curl -X DELETE http://localhost:5758/tasks/my-first-task
```

### 4. `POST /setTasks` - Synchronize tasks

This endpoint is typically used by controllers to synchronize a desired set of tasks. Tasks not present in the desired list will be marked for deletion; new tasks will be created.

*   **Method:** `POST`
*   **Path:** `/setTasks`
*   **Request Body (application/json):** An array of task objects representing the desired state.

    ```json
    [
      {
        "name": "task-alpha",
        "spec": {
          "process": {
            "command": ["sleep", "10"]
          }
        }
      },
      {
        "name": "task-beta",
        "spec": {
          "process": {
            "command": ["ls", "-l", "/tmp"]
          }
        }
      }
    ]
    ```

*   **Response Body (application/json):** The current list of tasks managed by the executor after synchronization.

    ```json
    [
      {
        "name": "task-alpha",
        "spec": {
          "process": {
            "command": ["sleep", "10"]
          }
        },
        "status": {
          "state": {
            "waiting": {
              "reason": "Initialized"
            }
          }
        }
      },
      {
        "name": "task-beta",
        "spec": {
          "process": {
            "command": ["ls", "-l", "/tmp"]
          }
        },
        "status": {
          "state": {
            "waiting": {
              "reason": "Initialized"
            }
          }
        }
      }
    ]
    ```

**Example (using `curl`):**

```bash
curl -X POST -H "Content-Type: application/json" -d \
'[
  {
    "name": "task-alpha",
    "spec": { "process": { "command": ["sleep", "10"] } }
  },
  {
    "name": "task-beta",
    "spec": { "process": { "command": ["ls", "-l", "/tmp"] } }
  }
]' http://localhost:5758/setTasks
```

### 5. `GET /getTasks` - List all tasks

Retrieves a list of all tasks currently managed by the `task-executor`.

*   **Method:** `GET`
*   **Path:** `/getTasks`
*   **Response Body (application/json):** An array of task objects.

    ```json
    [
      {
        "name": "task-alpha",
        "spec": {
          "process": {
            "command": ["sleep", "10"]
          }
        },
        "status": {
          "state": {
            "running": {
              "startedAt": "2025-12-17T10:05:00Z"
            }
          }
        }
      },
      {
        "name": "task-beta",
        "spec": {
          "process": {
            "command": ["ls", "-l", "/tmp"]
          }
        },
        "status": {
          "state": {
            "terminated": {
              "exitCode": 0,
              "reason": "Succeeded",
              "startedAt": "2025-12-17T10:06:00Z",
              "finishedAt": "2025-12-17T10:06:01Z"
            }
          }
        }
      }
    ]
    ```

**Example (using `curl`):**

```bash
curl http://localhost:5758/getTasks
```

### 6. `GET /health` - Health check

Returns the health status of the `task-executor`.

*   **Method:** `GET`
*   **Path:** `/health`
*   **Response Body (application/json):**

    ```json
    {
      "status": "healthy"
    }
    ```

**Example (using `curl`):**

```bash
curl http://localhost:5758/health
```

## Task Specification (`TaskSpec`) Structure

The `spec` field within a task object (`api/v1alpha1.TaskSpec`) defines how the task should be executed. It currently supports `process` and `container` execution modes.

### Process Task Example

This mode executes a command directly as a process.

```json
{
  "name": "my-process-task",
  "spec": {
    "process": {
      "command": ["python3", "my_script.py"],
      "args": ["--config", "/etc/app/config.yaml"],
      "env": [
        { "name": "DEBUG_MODE", "value": "true" }
      ],
      "workingDir": "/app"
    }
  }
}
```

### Container Task Example (Placeholder/Future Feature)

This mode is intended for executing tasks within containers managed by the CRI runtime. Note that as per `internal/task-executor/runtime/container.go`, this mode might still be a placeholder.

```json
{
  "name": "my-container-task",
  "spec": {
    "container": {
      "image": "ubuntu:latest",
      "command": ["/bin/bash", "-c"],
      "args": ["apt update && apt install -y curl"],
      "env": [
        { "name": "http_proxy", "value": "http://myproxy.com:5758" }
      ],
      "volumeMounts": [
        {
          "name": "data-volume",
          "mountPath": "/data"
        }
      ]
    }
  }
}
```

## Task Status (`TaskStatus`) Structure

The `status` field within a task object (`internal/task-executor/types/Status` mapped to `api/v1alpha1.TaskStatus` for external API) provides details about the task's current execution state.

```json
{
  "name": "my-task",
  "spec": { ... },
  "status": {
    "state": {
      "waiting": {
        "reason": "Initialized"
      }
    },
    // or
    "state": {
      "running": {
        "startedAt": "2025-12-17T10:00:00Z"
      }
    },
    // or
    "state": {
      "terminated": {
        "exitCode": 0,
        "reason": "Succeeded",
        "message": "Task completed successfully",
        "startedAt": "2025-12-17T10:00:00Z",
        "finishedAt": "2025-12-17T10:00:05Z"
      }
    }
  }
}
```

**State Types:**

*   `waiting`: Task is pending execution.
*   `running`: Task is currently executing.
*   `terminated`: Task has finished (succeeded or failed).

## Example Scenario: Running a Sidecar Task

If `task-executor` is configured with `--enable-sidecar-mode=true` and `--main-container-name=my-main-app`, it can execute tasks within the PID namespace of `my-main-app`.

```bash
# Assume task-executor is running in sidecar mode on a pod with 'my-main-app'
# This task will execute 'ls /proc/self/ns' from within the main container's namespace
curl -X POST -H "Content-Type: application/json" -d '{
  "name": "sidecar-namespace-check",
  "spec": {
    "process": {
      "command": ["ls", "/proc/self/ns"]
    }
  }
}' http://localhost:5758/tasks
```

