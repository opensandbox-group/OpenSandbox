// Copyright 2026 Alibaba Group Holding Ltd.
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

package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
)

const (
	SinkFile = "file"
	SinkOSS  = "oss"

	InternalReconcileInterval  = 30 * time.Second
	InternalBatchMaxItems      = 256
	InternalBatchFlushInterval = time.Second
)

var clusterIDPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

type Config struct {
	NodeName             string
	ClusterID            string
	Sources              []string
	Sink                 string
	LogRoot              string
	StateDir             string
	StateMaxBytes        int64
	FilePath             string
	FileMaxBytes         int64
	FileMaxFiles         int
	FileMaxTotalBytes    int64
	FileRetention        time.Duration
	OSSEndpoint          string
	OSSBucket            string
	OSSKeyPrefix         string
	OSSAccessKeyID       string
	OSSAccessKeySecret   string
	OSSSessionToken      string
	MemoryBudgetBytes    int64
	PerSandboxQueueBytes int64
	PerSandboxRateLimit  float64
	MaxLineBytes         int
	PartialTimeout       time.Duration
	DropPolicy           string
	SinkTimeout          time.Duration
	RetryMaxInterval     time.Duration
	EndedStateRetention  time.Duration
	ServerAddr           string
	PprofAddr            string
}

type listenAddress struct {
	host string
	port int
}

func Load() (Config, error) {
	var errs []error
	cfg := Config{
		NodeName:           strings.TrimSpace(os.Getenv("NODE_NAME")),
		ClusterID:          strings.TrimSpace(os.Getenv("NODEAGENT_CLUSTER_ID")),
		Sources:            parseSources(envDefault("NODEAGENT_SOURCES", api.SourceNameContainerLogs), &errs),
		Sink:               envDefault("NODEAGENT_SINKS", SinkFile),
		LogRoot:            envDefault("NODEAGENT_LOG_ROOT", "/var/log/pods"),
		StateDir:           envDefault("NODEAGENT_STATE_DIR", "/var/lib/opensandbox/nodeagent"),
		FilePath:           strings.TrimSpace(os.Getenv("NODEAGENT_FILE_PATH")),
		OSSEndpoint:        strings.TrimSpace(os.Getenv("NODEAGENT_OSS_ENDPOINT")),
		OSSBucket:          strings.TrimSpace(os.Getenv("NODEAGENT_OSS_BUCKET")),
		OSSKeyPrefix:       strings.Trim(strings.TrimSpace(os.Getenv("NODEAGENT_OSS_KEY_PREFIX")), "/"),
		OSSAccessKeyID:     strings.TrimSpace(os.Getenv("OSS_ACCESS_KEY_ID")),
		OSSAccessKeySecret: strings.TrimSpace(os.Getenv("OSS_ACCESS_KEY_SECRET")),
		OSSSessionToken:    strings.TrimSpace(os.Getenv("OSS_SESSION_TOKEN")),
		DropPolicy:         envDefault("NODEAGENT_DROP_POLICY", "block"),
		ServerAddr:         envDefault("NODEAGENT_SERVER_ADDR", ":8080"),
		PprofAddr:          strings.TrimSpace(os.Getenv("NODEAGENT_PPROF_ADDR")),
	}

	cfg.StateMaxBytes = parseInt64("NODEAGENT_STATE_MAX_BYTES", 1<<30, true, &errs)
	cfg.MemoryBudgetBytes = parseInt64("NODEAGENT_MEMORY_BUDGET_BYTES", 256<<20, true, &errs)
	cfg.PerSandboxQueueBytes = parseInt64("NODEAGENT_PER_SANDBOX_QUEUE_BYTES", 16<<20, true, &errs)
	cfg.PerSandboxRateLimit = parseFloat("NODEAGENT_PER_SANDBOX_RATE_LIMIT", 0, false, &errs)
	cfg.SinkTimeout = parseDuration("NODEAGENT_SINK_TIMEOUT", 30*time.Second, true, &errs)
	cfg.RetryMaxInterval = parseDuration("NODEAGENT_RETRY_MAX_INTERVAL", 30*time.Second, true, &errs)
	if cfg.HasSource(api.SourceNameContainerLogs) {
		cfg.MaxLineBytes = int(parseInt64("NODEAGENT_MAX_LINE_BYTES", 1<<20, true, &errs))
		cfg.PartialTimeout = parseDuration("NODEAGENT_PARTIAL_TIMEOUT", 5*time.Second, true, &errs)
		cfg.EndedStateRetention = parseDuration("NODEAGENT_ENDED_STATE_RETENTION", 24*time.Hour, true, &errs)
	}
	if cfg.Sink == SinkFile {
		cfg.FileMaxBytes = parseInt64("NODEAGENT_FILE_MAX_BYTES", 1<<30, true, &errs)
		cfg.FileMaxFiles = int(parseInt64("NODEAGENT_FILE_MAX_FILES", 16, true, &errs))
		cfg.FileMaxTotalBytes = parseInt64("NODEAGENT_FILE_MAX_TOTAL_BYTES", 10<<30, true, &errs)
		cfg.FileRetention = parseDuration("NODEAGENT_FILE_RETENTION", 24*time.Hour, false, &errs)
	} else if cfg.Sink == SinkOSS && cfg.OSSEndpoint != "" {
		canonical, err := identity.CanonicalOSSEndpoint(cfg.OSSEndpoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("NODEAGENT_OSS_ENDPOINT: %w", err))
		} else {
			cfg.OSSEndpoint = canonical
		}
	}

	errs = append(errs, cfg.validate()...)
	return cfg, errors.Join(errs...)
}

func (c Config) validate() []error {
	var errs []error
	if len(c.Sources) == 0 {
		errs = append(errs, errors.New("NODEAGENT_SOURCES must contain at least one Source"))
	}
	if c.NodeName == "" {
		errs = append(errs, errors.New("NODE_NAME is required"))
	}
	if !clusterIDPattern.MatchString(c.ClusterID) {
		errs = append(errs, errors.New("NODEAGENT_CLUSTER_ID must be a DNS label"))
	}
	if c.DropPolicy != "block" && c.DropPolicy != "drop" {
		errs = append(errs, errors.New("NODEAGENT_DROP_POLICY must be block or drop"))
	}
	if err := validateAbsolutePath(c.StateDir); err != nil {
		errs = append(errs, fmt.Errorf("NODEAGENT_STATE_DIR: %w", err))
	}
	containerLogsEnabled := c.HasSource(api.SourceNameContainerLogs)
	if containerLogsEnabled {
		if err := validateAbsolutePath(c.LogRoot); err != nil {
			errs = append(errs, fmt.Errorf("NODEAGENT_LOG_ROOT: %w", err))
		}
		if pathsOverlap(c.StateDir, c.LogRoot) {
			errs = append(errs, errors.New("NODEAGENT_STATE_DIR must not overlap NODEAGENT_LOG_ROOT"))
		}
	}
	switch c.Sink {
	case SinkFile:
		if c.FilePath != "" {
			if err := validateAbsolutePath(c.FilePath); err != nil {
				errs = append(errs, fmt.Errorf("NODEAGENT_FILE_PATH: %w", err))
			}
			if pathsOverlap(c.FilePath, c.StateDir) || containerLogsEnabled && pathsOverlap(c.FilePath, c.LogRoot) {
				errs = append(errs, errors.New("NODEAGENT_FILE_PATH must not overlap active state or source paths"))
			}
			if c.FileMaxTotalBytes < c.FileMaxBytes {
				errs = append(errs, errors.New("NODEAGENT_FILE_MAX_TOTAL_BYTES cannot be smaller than NODEAGENT_FILE_MAX_BYTES"))
			}
		}
	case SinkOSS:
		if c.OSSEndpoint == "" || c.OSSBucket == "" || c.OSSAccessKeyID == "" || c.OSSAccessKeySecret == "" {
			errs = append(errs, errors.New("OSS endpoint, bucket, access key ID, and access key secret are required"))
		}
		if c.OSSKeyPrefix == "" || unsafeObjectPrefix(c.OSSKeyPrefix) {
			errs = append(errs, errors.New("NODEAGENT_OSS_KEY_PREFIX must be a non-empty safe object prefix"))
		}
	}
	if c.PerSandboxQueueBytes > c.MemoryBudgetBytes {
		errs = append(errs, errors.New("per-sandbox queue budget cannot exceed global memory budget"))
	}
	if containerLogsEnabled {
		if c.MaxLineBytes > 1<<30 {
			errs = append(errs, errors.New("NODEAGENT_MAX_LINE_BYTES must not exceed 1 GiB"))
		} else if int64(c.MaxLineBytes)+512 > c.PerSandboxQueueBytes {
			errs = append(errs, errors.New("NODEAGENT_MAX_LINE_BYTES plus record overhead must fit the per-sandbox queue budget"))
		}
	}
	if c.FileMaxFiles > 1<<20 {
		errs = append(errs, errors.New("file-count limit must not exceed 1048576"))
	}
	serverAddress, serverErr := parseListenAddress(c.ServerAddr)
	if serverErr != nil {
		errs = append(errs, fmt.Errorf("NODEAGENT_SERVER_ADDR: %w", serverErr))
	}
	var pprofAddress listenAddress
	var pprofErr error
	if c.PprofAddr != "" {
		pprofAddress, pprofErr = parseListenAddress(c.PprofAddr)
		if pprofErr != nil {
			errs = append(errs, fmt.Errorf("NODEAGENT_PPROF_ADDR: %w", pprofErr))
		}
	}
	if c.PprofAddr != "" && serverErr == nil && pprofErr == nil && listenAddressesConflict(serverAddress, pprofAddress) {
		errs = append(errs, errors.New("NODEAGENT_PPROF_ADDR must not conflict with NODEAGENT_SERVER_ADDR"))
	}
	if c.PprofAddr != "" && pprofErr == nil {
		host := pprofAddress.host
		if !strings.EqualFold(host, "localhost") {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				errs = append(errs, errors.New("NODEAGENT_PPROF_ADDR must bind to a loopback address"))
			}
		}
	}
	return errs
}

func parseSources(raw string, errs *[]error) []string {
	parts := strings.Split(raw, ",")
	sources := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			*errs = append(*errs, errors.New("NODEAGENT_SOURCES contains an empty Source name"))
			continue
		}
		if _, exists := seen[name]; exists {
			*errs = append(*errs, fmt.Errorf("NODEAGENT_SOURCES contains duplicate Source %q", name))
			continue
		}
		seen[name] = struct{}{}
		sources = append(sources, name)
	}
	return sources
}

// HasSource reports whether name is enabled in the configured Source set.
func (c Config) HasSource(name string) bool {
	for _, source := range c.Sources {
		if source == name {
			return true
		}
	}
	return false
}

func parseListenAddress(address string) (listenAddress, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return listenAddress{}, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return listenAddress{}, fmt.Errorf("invalid port %q: %w", port, err)
	}
	if portNumber < 1 || portNumber > 65535 {
		return listenAddress{}, fmt.Errorf("listen port %d must be between 1 and 65535", portNumber)
	}
	return listenAddress{host: host, port: portNumber}, nil
}

func listenAddressesConflict(left, right listenAddress) bool {
	if left.port != right.port {
		return false
	}
	if wildcardHost(left.host) || wildcardHost(right.host) {
		return true
	}
	if strings.EqualFold(left.host, right.host) {
		return true
	}
	leftIP, rightIP := net.ParseIP(left.host), net.ParseIP(right.host)
	if strings.EqualFold(left.host, "localhost") && rightIP != nil && rightIP.IsLoopback() ||
		strings.EqualFold(right.host, "localhost") && leftIP != nil && leftIP.IsLoopback() {
		return true
	}
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
}

func wildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	separator := string(filepath.Separator)
	return strings.HasPrefix(left, right+separator) || strings.HasPrefix(right, left+separator)
}

func unsafeObjectPrefix(prefix string) bool {
	if strings.Contains(prefix, "\\") {
		return true
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func validateAbsolutePath(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("must be an absolute path")
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || strings.Contains(path, "*") {
		return errors.New("root, glob, and path traversal are not allowed")
	}
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." {
			return errors.New("root, glob, and path traversal are not allowed")
		}
	}
	return nil
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseInt64(key string, fallback int64, positive bool, errs *[]error) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseUint(raw, 10, 63)
	if err != nil || positive && value == 0 {
		*errs = append(*errs, fmt.Errorf("%s must be an unsigned decimal%s", key, map[bool]string{true: " greater than zero"}[positive]))
		return fallback
	}
	return int64(value)
}

func parseFloat(key string, fallback float64, positive bool, errs *[]error) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || positive && value == 0 {
		*errs = append(*errs, fmt.Errorf("%s has an invalid numeric value", key))
		return fallback
	}
	return value
}

func parseDuration(key string, fallback time.Duration, positive bool, errs *[]error) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 || positive && value == 0 {
		*errs = append(*errs, fmt.Errorf("%s has an invalid duration", key))
		return fallback
	}
	return value
}
