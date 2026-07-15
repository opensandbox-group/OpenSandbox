# Logging Configuration

## Features

The OpenSandbox Kubernetes Controller supports flexible logging configuration, including:

- Console log output (enabled by default)
- File log output (optional)
- Automatic log rotation (by file size)
- Automatic compression of old logs (gzip)
- Automatic cleanup of expired logs (by age or count)
- All standard zap options (log level, format, etc.)

## Command-Line Flags

### Log File Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--enable-file-log` | bool | false | Enable log output to file |
| `--log-file-path` | string | `/var/log/sandbox-controller/controller.log` | Log file path |
| `--log-max-size` | int | 100 | Maximum size of a single log file in MB; rotates when exceeded |
| `--log-max-backups` | int | 10 | Maximum number of old log files to retain |
| `--log-max-age` | int | 30 | Maximum number of days to retain old log files |
| `--log-compress` | bool | true | Compress rotated log files (gzip) |

### Standard zap Flags (inherited from controller-runtime)

| Flag | Description |
|------|-------------|
| `--zap-devel` | Enable development mode (colorized output, more verbose stack traces) |
| `--zap-encoder` | Log encoding format: json or console |
| `--zap-log-level` | Log level: debug, info, error, etc. |
| `--zap-stacktrace-level` | Minimum log level at which stack traces are printed |
| `--zap-time-encoding` | Time encoding format: iso8601, millis, nano, etc. |

## Usage Examples

### 1. Console Output Only (Default)

```bash
./controller
```

### 2. Output to Both Console and File

```bash
./controller \
  --enable-file-log=true \
  --log-file-path=/var/log/sandbox-controller/controller.log
```

### 3. Custom Log Rotation Configuration

```bash
./controller \
  --enable-file-log=true \
  --log-file-path=/var/log/sandbox-controller/controller.log \
  --log-max-size=50 \
  --log-max-backups=5 \
  --log-max-age=7 \
  --log-compress=true
```

This configuration:
- Each log file can be up to 50MB
- Retain at most 5 old log files
- Retain log files for at most 7 days
- Compress old log files

### 4. Development Mode + File Output

```bash
./controller \
  --zap-devel=true \
  --enable-file-log=true \
  --log-file-path=/tmp/controller-dev.log
```

### 5. JSON Format + File Output

```bash
./controller \
  --zap-encoder=json \
  --enable-file-log=true \
  --log-file-path=/var/log/sandbox-controller/controller.log
```

### 6. Debug Level + File Output

```bash
./controller \
  --zap-log-level=debug \
  --enable-file-log=true \
  --log-file-path=/var/log/sandbox-controller/debug.log
```

## Kubernetes Deployment Configuration

When deploying in Kubernetes, configure logging options via the Deployment's `args`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sandbox-controller
spec:
  template:
    spec:
      containers:
      - name: controller
        image: sandbox-controller:latest
        args:
        - --enable-file-log=true
        - --log-file-path=/var/log/controller/controller.log
        - --log-max-size=100
        - --log-max-backups=10
        - --log-max-age=30
        - --log-compress=true
        - --zap-encoder=json
        volumeMounts:
        - name: log-volume
          mountPath: /var/log/controller
      volumes:
      - name: log-volume
        emptyDir: {}
        # Or use a PersistentVolumeClaim
        # persistentVolumeClaim:
        #   claimName: controller-logs
```

## Log File Format

### Development Mode (--zap-devel=true)

```
2026-02-12T10:30:45.123+0800	INFO	setup	starting manager
2026-02-12T10:30:45.456+0800	INFO	controller	Reconciling	{"namespace": "default", "name": "example"}
```

### Production Mode (JSON)

```json
{"level":"info","ts":"2026-02-12T10:30:45.123+0800","logger":"setup","msg":"starting manager"}
{"level":"info","ts":"2026-02-12T10:30:45.456+0800","logger":"controller","msg":"Reconciling","namespace":"default","name":"example"}
```

## Log Rotation Mechanism

Log rotation is implemented by [lumberjack](https://github.com/natefinch/lumberjack) and supports:

1. **Size-based rotation**: When a log file reaches the size specified by `--log-max-size`, a new file is automatically created
2. **File naming**: Rotated files are named in the format `controller.log.2026-02-12T10-30-45.123`
3. **Automatic compression**: If `--log-compress` is enabled, old log files are compressed to `.gz` format
4. **Automatic cleanup**:
   - Retain the most recent N files based on `--log-max-backups`
   - Delete files older than the specified number of days based on `--log-max-age`

## Directory Permissions

Ensure the log directory exists and has write permissions:

```bash
# Create the log directory
mkdir -p /var/log/sandbox-controller

# Set permissions (adjust based on the actual runtime user)
chown controller:controller /var/log/sandbox-controller
chmod 755 /var/log/sandbox-controller
```

In Kubernetes, you can use an `initContainer` or `securityContext` to ensure correct permissions:

```yaml
spec:
  initContainers:
  - name: setup-log-dir
    image: busybox
    command: ['sh', '-c', 'mkdir -p /var/log/controller && chmod 755 /var/log/controller']
    volumeMounts:
    - name: log-volume
      mountPath: /var/log/controller
  containers:
  - name: controller
    securityContext:
      runAsUser: 1000
      runAsGroup: 1000
```

## Monitoring and Viewing Logs

### View Current Logs

```bash
tail -f /var/log/sandbox-controller/controller.log
```

### View Compressed Logs

```bash
zcat /var/log/sandbox-controller/controller.log.2026-02-12T10-30-45.123.gz | less
```

### Search Logs

```bash
# Search for error logs
grep -i error /var/log/sandbox-controller/controller.log

# Search across all log files (including compressed)
zgrep -i error /var/log/sandbox-controller/*.log*
```

## Best Practices

1. **Production environment**:
   ```bash
   --enable-file-log=true
   --log-file-path=/var/log/sandbox-controller/controller.log
   --log-max-size=100
   --log-max-backups=10
   --log-max-age=30
   --log-compress=true
   --zap-encoder=json
   ```

2. **Development environment**:
   ```bash
   --zap-devel=true
   --enable-file-log=true
   --log-file-path=/tmp/controller-dev.log
   --log-compress=false
   ```

3. **Debugging issues**:
   ```bash
   --zap-log-level=debug
   --enable-file-log=true
   --log-max-size=500
   --log-compress=false
   ```

4. **Limited disk space**:
   ```bash
   --enable-file-log=true
   --log-max-size=50
   --log-max-backups=3
   --log-max-age=7
   --log-compress=true
   ```

## Troubleshooting

### Log File Not Created

1. Check if the directory exists: `ls -la /var/log/sandbox-controller/`
2. Check permissions: `ls -ld /var/log/sandbox-controller/`
3. Verify the process has write permissions
4. Check controller startup logs for errors

### Log File Not Rotating

1. Confirm `--enable-file-log=true` is set
2. Check if the file size has reached the `--log-max-size` limit
3. Verify the lumberjack library is correctly installed: `go list -m gopkg.in/natefinch/lumberjack.v2`

### Excessive Disk Space Usage

1. Reduce the value of `--log-max-size`
2. Reduce the number of `--log-max-backups`
3. Reduce the number of days in `--log-max-age`
4. Ensure `--log-compress=true` is enabled
