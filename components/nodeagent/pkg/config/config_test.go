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
	"strings"
	"testing"
)

func TestLoadFileConfig(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
	t.Setenv("NODEAGENT_SINKS", "file")
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("NODEAGENT_FILE_PATH", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.NodeName != "node-1" || cfg.ClusterID != "prod-a" || cfg.Sink != SinkFile {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if got, want := strings.Join(cfg.Sources, ","), "container-logs"; got != want {
		t.Fatalf("Sources = %q, want %q", got, want)
	}
}

func TestLoadParsesMultipleSources(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
	t.Setenv("NODEAGENT_SOURCES", " container-logs , syscalls ")
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := strings.Join(cfg.Sources, ","), "container-logs,syscalls"; got != want {
		t.Fatalf("Sources = %q, want %q", got, want)
	}
}

func TestLoadRejectsEmptyOrDuplicateSources(t *testing.T) {
	for _, value := range []string{"container-logs,,syscalls", ",container-logs", "container-logs,container-logs"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("NODE_NAME", "node-1")
			t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
			t.Setenv("NODEAGENT_SOURCES", value)
			t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
			t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NODEAGENT_SOURCES") {
				t.Fatalf("Load() error = %v, want NODEAGENT_SOURCES error", err)
			}
		})
	}
}

func TestLoadDoesNotValidateDisabledContainerLogSettings(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
	t.Setenv("NODEAGENT_SOURCES", "custom-source")
	t.Setenv("NODEAGENT_LOG_ROOT", "relative")
	t.Setenv("NODEAGENT_MAX_LINE_BYTES", "invalid")
	t.Setenv("NODEAGENT_PARTIAL_TIMEOUT", "invalid")
	t.Setenv("NODEAGENT_ENDED_STATE_RETENTION", "invalid")
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())

	if _, err := Load(); err != nil {
		t.Fatalf("Load() validated settings for a disabled Source: %v", err)
	}
}

func TestLoadRejectsInvalidIdentityAndBudget(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "INVALID")
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("NODEAGENT_MEMORY_BUDGET_BYTES", "10")
	t.Setenv("NODEAGENT_PER_SANDBOX_QUEUE_BYTES", "11")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestValidateRejectsConflictingServerAddresses(t *testing.T) {
	tests := []struct {
		name       string
		serverAddr string
		pprofAddr  string
	}{
		{name: "empty wildcard", serverAddr: ":8080", pprofAddr: "127.0.0.1:8080"},
		{name: "IPv4 wildcard", serverAddr: "0.0.0.0:8080", pprofAddr: "127.0.0.1:8080"},
		{name: "IPv6 wildcard", serverAddr: "[::]:8080", pprofAddr: "[::1]:8080"},
		{name: "localhost alias", serverAddr: "localhost:8080", pprofAddr: "127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ServerAddr = tt.serverAddr
			cfg.PprofAddr = tt.pprofAddr
			if err := errorsContaining(cfg.validate(), "must not conflict"); err == "" {
				t.Fatalf("validate() accepted conflicting addresses %q and %q", tt.serverAddr, tt.pprofAddr)
			}
		})
	}
}

func TestValidateRejectsInvalidListenPort(t *testing.T) {
	for _, address := range []string{":not-a-port", ":http", "localhost:", ":0", ":70000"} {
		cfg := validConfig()
		cfg.ServerAddr = address
		if err := errorsContaining(cfg.validate(), "NODEAGENT_SERVER_ADDR"); err == "" {
			t.Fatalf("validate() accepted invalid listen address %q", address)
		}
	}
}

func TestValidateAbsolutePathRejectsSegmentsNotNames(t *testing.T) {
	if err := validateAbsolutePath("/var/lib/nodeagent..data"); err != nil {
		t.Fatalf("validateAbsolutePath() rejected a safe name: %v", err)
	}
	for _, value := range []string{"/", "/var/lib/../etc", "/var/*/data"} {
		if err := validateAbsolutePath(value); err == nil {
			t.Fatalf("validateAbsolutePath(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLoadValidatesOSSEndpoint(t *testing.T) {
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("NODEAGENT_CLUSTER_ID", "prod-a")
	t.Setenv("NODEAGENT_SINKS", SinkOSS)
	t.Setenv("NODEAGENT_LOG_ROOT", t.TempDir())
	t.Setenv("NODEAGENT_STATE_DIR", t.TempDir())
	t.Setenv("NODEAGENT_OSS_ENDPOINT", "HTTPS://OSS.Example.COM:443/")
	t.Setenv("NODEAGENT_OSS_BUCKET", "bucket")
	t.Setenv("NODEAGENT_OSS_KEY_PREFIX", "logs")
	t.Setenv("OSS_ACCESS_KEY_ID", "id")
	t.Setenv("OSS_ACCESS_KEY_SECRET", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OSSEndpoint != "https://oss.example.com" {
		t.Fatalf("OSS endpoint = %q", cfg.OSSEndpoint)
	}
	t.Setenv("NODEAGENT_OSS_ENDPOINT", "http://oss.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NODEAGENT_OSS_ENDPOINT") {
		t.Fatalf("Load() error = %v, want endpoint setting context", err)
	}
}

func TestValidateAllowsCompiledExtensionNames(t *testing.T) {
	cfg := validConfig()
	cfg.Sources = []string{"custom-source"}
	cfg.Sink = "custom-sink"
	if errs := cfg.validate(); len(errs) != 0 {
		t.Fatalf("validate() rejected extension selectors: %v", errs)
	}
}

func validConfig() Config {
	return Config{
		NodeName:             "node-1",
		ClusterID:            "prod-a",
		Sources:              []string{"container-logs"},
		Sink:                 SinkFile,
		LogRoot:              "/var/log/pods",
		StateDir:             "/var/lib/opensandbox/nodeagent",
		MemoryBudgetBytes:    1024,
		PerSandboxQueueBytes: 1024,
		MaxLineBytes:         1,
		DropPolicy:           "block",
		FileMaxFiles:         1,
		ServerAddr:           ":8080",
	}
}

func errorsContaining(errs []error, substring string) string {
	for _, err := range errs {
		if strings.Contains(err.Error(), substring) {
			return err.Error()
		}
	}
	return ""
}
