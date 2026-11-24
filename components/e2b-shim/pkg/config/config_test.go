// Copyright 2026 The OpenSandbox Authors
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

package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/e2b-shim/pkg/config"
)

// Obviously fake secret values. Real credentials never appear in tests.
const (
	fakeAdapterKey = "fake-adapter-api-key-DO-NOT-USE"
	fakeNativeKey  = "fake-native-api-key-DO-NOT-USE"
	fakeHMACKey    = "fake-hmac-key-DO-NOT-USE"
)

const validFile = `{
  "listen": "127.0.0.1:18080",
  "compat_prefix": "/compat/e2b",
  "native_api_url": "https://native.example.internal/v1",
  "adapter_id": "e2b-shim-test",
  "domain": "sandbox.example.internal",
  "hmac_key_version": "v1",
  "timeout_default_seconds": 300,
  "timeout_min_seconds": 60,
  "timeout_max_seconds": 3600,
  "templates": [
    {
      "template_id": "base",
      "aliases": ["base-envd"],
      "native_template_id": "tpl-base",
      "revision": "tpl-base@rev-1",
      "expected_envd_version": "0.8.0"
    }
  ]
}`

func secretEnv() map[string]string {
	return map[string]string{
		"E2B_SHIM_ADAPTER_API_KEY": fakeAdapterKey,
		"E2B_SHIM_NATIVE_API_KEY":  fakeNativeKey,
		"E2B_SHIM_HMAC_KEY":        fakeHMACKey,
	}
}

// envWith returns a copy of the secret environment with overrides applied.
func envWith(overrides map[string]string) map[string]string {
	env := secretEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "e2b-shim.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, src config.Source, env map[string]string) *config.Config {
	t.Helper()
	if env == nil {
		env = secretEnv()
	}
	src.LookupEnv = lookup(env)
	cfg, err := config.Load(src)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	return cfg
}

func TestLoadFile(t *testing.T) {
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, validFile)}, nil)
	if cfg.Listen != "127.0.0.1:18080" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.CompatPrefix != "/compat/e2b" {
		t.Errorf("compat_prefix = %q", cfg.CompatPrefix)
	}
	if cfg.NativeAPIURL != "https://native.example.internal/v1" {
		t.Errorf("native_api_url = %q", cfg.NativeAPIURL)
	}
	if cfg.AdapterID != "e2b-shim-test" || cfg.HMACKeyVersion != "v1" || cfg.Domain != "sandbox.example.internal" {
		t.Errorf("string fields = %q, %q, %q", cfg.AdapterID, cfg.HMACKeyVersion, cfg.Domain)
	}
	if cfg.TimeoutDefaultSeconds != 300 || cfg.TimeoutMinSeconds != 60 || cfg.TimeoutMaxSeconds != 3600 {
		t.Errorf("timeouts = %d, %d, %d", cfg.TimeoutDefaultSeconds, cfg.TimeoutMinSeconds, cfg.TimeoutMaxSeconds)
	}
	if len(cfg.Templates) != 1 || cfg.Templates[0].TemplateID != "base" || cfg.Templates[0].Aliases[0] != "base-envd" {
		t.Errorf("templates = %+v", cfg.Templates)
	}
	if !cfg.AdapterAPIKey.Equal(fakeAdapterKey) || !cfg.NativeAPIKey.Equal(fakeNativeKey) || !cfg.HMACKey.Equal(fakeHMACKey) {
		t.Error("secrets not resolved from environment")
	}
	if cfg.AdapterAPIKey.Equal("wrong-candidate") || cfg.AdapterAPIKey.Equal("") || (config.Secret{}).Equal("") {
		t.Error("Equal accepted a wrong candidate")
	}
}

// requiredEnv supplies the fields with no defaults plus the secrets.
func requiredEnv() map[string]string {
	return envWith(map[string]string{
		"E2B_SHIM_NATIVE_API_URL":      "https://native.example.internal",
		"E2B_SHIM_ADAPTER_ID":          "e2b-shim-test",
		"E2B_SHIM_HMAC_KEY_VERSION":    "v1",
		"E2B_SHIM_TIMEOUT_MAX_SECONDS": "3600",
	})
}

func TestLoadDefaults(t *testing.T) {
	cfg := mustLoad(t, config.Source{}, requiredEnv())
	if cfg.Listen != config.DefaultListen || cfg.CompatPrefix != config.DefaultCompatPrefix {
		t.Errorf("defaults = %q, %q", cfg.Listen, cfg.CompatPrefix)
	}
	if cfg.TimeoutDefaultSeconds != 300 || cfg.TimeoutMinSeconds != 60 {
		t.Errorf("timeout defaults = %d, %d", cfg.TimeoutDefaultSeconds, cfg.TimeoutMinSeconds)
	}
	if len(cfg.Templates) != 0 {
		t.Errorf("templates = %+v", cfg.Templates)
	}
}

func TestLoadNormalizesTrailingSlashes(t *testing.T) {
	file := strings.Replace(validFile, `"native_api_url": "https://native.example.internal/v1"`,
		`"native_api_url": "https://native.example.internal/v1/"`, 1)
	file = strings.Replace(file, `"compat_prefix": "/compat/e2b"`, `"compat_prefix": "/compat/e2b/"`, 1)
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, file)}, nil)
	if cfg.CompatPrefix != "/compat/e2b" || cfg.NativeAPIURL != "https://native.example.internal/v1" {
		t.Errorf("normalized = %q, %q", cfg.CompatPrefix, cfg.NativeAPIURL)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	env := envWith(map[string]string{
		"E2B_SHIM_LISTEN":                  "127.0.0.1:19090",
		"E2B_SHIM_COMPAT_PREFIX":           "/compat/e2b-test",
		"E2B_SHIM_NATIVE_API_URL":          "http://native2.example.internal",
		"E2B_SHIM_ADAPTER_ID":              "adapter-2",
		"E2B_SHIM_DOMAIN":                  "sbx2.example.internal",
		"E2B_SHIM_HMAC_KEY_VERSION":        "v2",
		"E2B_SHIM_TIMEOUT_DEFAULT_SECONDS": "120",
		"E2B_SHIM_TIMEOUT_MIN_SECONDS":     "30",
		"E2B_SHIM_TIMEOUT_MAX_SECONDS":     "600",
		"E2B_SHIM_TEMPLATES":               `[{"template_id":"t2","native_template_id":"tpl2","revision":"tpl2@r1","expected_envd_version":"0.8.0"}]`,
	})
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, validFile)}, env)
	if cfg.Listen != "127.0.0.1:19090" || cfg.CompatPrefix != "/compat/e2b-test" || cfg.AdapterID != "adapter-2" ||
		cfg.Domain != "sbx2.example.internal" || cfg.HMACKeyVersion != "v2" {
		t.Errorf("string overrides not applied: %q, %q, %q", cfg.Listen, cfg.CompatPrefix, cfg.AdapterID)
	}
	if cfg.NativeAPIURL != "http://native2.example.internal" {
		t.Errorf("native_api_url = %q", cfg.NativeAPIURL)
	}
	if cfg.TimeoutDefaultSeconds != 120 || cfg.TimeoutMinSeconds != 30 || cfg.TimeoutMaxSeconds != 600 {
		t.Errorf("timeout overrides = %d, %d, %d", cfg.TimeoutDefaultSeconds, cfg.TimeoutMinSeconds, cfg.TimeoutMaxSeconds)
	}
	if len(cfg.Templates) != 1 || cfg.Templates[0].TemplateID != "t2" {
		t.Errorf("templates override = %+v", cfg.Templates)
	}
}

func TestLoadListenFlagOverridesEnv(t *testing.T) {
	listen := "127.0.0.1:19191"
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, validFile), Listen: &listen}, envWith(map[string]string{
		"E2B_SHIM_LISTEN": "127.0.0.1:19090",
	}))
	if cfg.Listen != listen {
		t.Errorf("listen = %q, want %q", cfg.Listen, listen)
	}
}

func TestLoadListenFlagSkipsLowerPrioritySources(t *testing.T) {
	listen := "127.0.0.1:19292"
	for _, file := range []string{`"listen": null`, `"listen": 8080`, `"listen": "8080"`} {
		data := strings.Replace(validFile, `"listen": "127.0.0.1:18080"`, file, 1)
		cfg := mustLoad(t, config.Source{Path: writeConfig(t, data), Listen: &listen}, envWith(nil))
		if cfg.Listen != listen {
			t.Errorf("file %s: listen = %q, want %q", file, cfg.Listen, listen)
		}
	}
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, validFile), Listen: &listen}, envWith(map[string]string{
		"E2B_SHIM_LISTEN": "8080",
	}))
	if cfg.Listen != listen {
		t.Errorf("invalid env: listen = %q, want %q", cfg.Listen, listen)
	}
}

func TestLoadBoundaryValues(t *testing.T) {
	env := requiredEnv()
	env["E2B_SHIM_LISTEN"] = ":0"
	env["E2B_SHIM_NATIVE_API_URL"] = "https://[::1]:65535/v1"
	env["E2B_SHIM_TIMEOUT_MIN_SECONDS"] = "9223372036"
	env["E2B_SHIM_TIMEOUT_DEFAULT_SECONDS"] = "9223372036"
	env["E2B_SHIM_TIMEOUT_MAX_SECONDS"] = "9223372036"
	cfg := mustLoad(t, config.Source{}, env)
	if cfg.Listen != ":0" || cfg.NativeAPIURL != "https://[::1]:65535/v1" {
		t.Errorf("listen = %q, native API URL = %q", cfg.Listen, cfg.NativeAPIURL)
	}
	if cfg.TimeoutMinSeconds != 9223372036 || cfg.TimeoutDefaultSeconds != 9223372036 || cfg.TimeoutMaxSeconds != 9223372036 {
		t.Errorf("timeouts = %d, %d, %d", cfg.TimeoutMinSeconds, cfg.TimeoutDefaultSeconds, cfg.TimeoutMaxSeconds)
	}
}

func TestLoadNullAliases(t *testing.T) {
	file := strings.Replace(validFile, `"aliases": ["base-envd"]`, `"aliases": null`, 1)
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, file)}, nil)
	if len(cfg.Templates) != 1 || len(cfg.Templates[0].Aliases) != 0 {
		t.Errorf("templates = %+v, want one mapping with no aliases", cfg.Templates)
	}
}

func TestSecretsFromFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "hmac-key")
	if err := os.WriteFile(keyFile, []byte(fakeHMACKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	crlfFile := filepath.Join(dir, "native-key")
	if err := os.WriteFile(crlfFile, []byte(fakeNativeKey+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A double newline keeps the second newline: only one is stripped.
	doubleFile := filepath.Join(dir, "adapter-key")
	if err := os.WriteFile(doubleFile, []byte(fakeAdapterKey+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	file := `{
  "native_api_url": "https://native.example.internal/v1",
  "adapter_id": "e2b-shim-test",
  "hmac_key_version": "v1",
  "timeout_max_seconds": 3600,
  "adapter_api_key": {"file": "` + doubleFile + `"},
  "native_api_key": {"file": "` + crlfFile + `"},
  "hmac_key": {"env": "FAKE_HMAC_ENV"}
}`
	// Direct environment overrides would win over the file references, so
	// only the referenced variable is set.
	env := map[string]string{"FAKE_HMAC_ENV": fakeHMACKey}
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, file)}, env)
	if !cfg.HMACKey.Equal(fakeHMACKey) {
		t.Error("env reference not resolved")
	}
	if !cfg.NativeAPIKey.Equal(fakeNativeKey) {
		t.Error("CRLF trailing newline not stripped")
	}
	if !cfg.AdapterAPIKey.Equal(fakeAdapterKey + "\n") {
		t.Error("second trailing newline must be kept")
	}
}

func TestLoadFailures(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "missing-key")
	cases := []struct {
		name      string
		file      string
		env       map[string]string
		listen    *string
		wantErr   string
		doNotEcho string
	}{
		{name: "missing config file", file: filepath.Join(dir, "nope.json"), wantErr: "config file: is not readable", doNotEcho: "nope.json"},
		{name: "unknown field", file: `{"bogus": 1}`, wantErr: `unknown field "bogus"`},
		{name: "trailing data", file: validFile + " {}", wantErr: "unexpected data after JSON value"},
		{name: "incomplete JSON", file: `{`, wantErr: "incomplete JSON"},
		{name: "null field", file: `{"listen": null}`, wantErr: "listen: must not be null"},
		{name: "null templates", file: `{"templates": null}`, wantErr: "templates: must not be null"},
		{name: "wrong type", file: `{"timeout_default_seconds": "300"}`, wantErr: "timeout_default_seconds: wrong type"},
		{name: "env integer parse", env: envWith(map[string]string{"E2B_SHIM_TIMEOUT_MAX_SECONDS": "3600.5"}), wantErr: "is not a base-10 integer"},
		{name: "env empty listen", file: validFile, env: envWith(map[string]string{"E2B_SHIM_LISTEN": ""}), wantErr: "listen: must be host:port"},
		{name: "empty flag listen", file: validFile, env: envWith(map[string]string{"E2B_SHIM_LISTEN": "127.0.0.1:18080"}), listen: ptr(""), wantErr: "listen: must be host:port"},
		{name: "invalid listen host", file: `{"listen": "8080"}`, wantErr: "listen: must be host:port"},
		{name: "invalid listen port", file: `{"listen": "127.0.0.1:http"}`, wantErr: "listen: port must be a decimal"},
		{name: "padded listen port", file: `{"listen": "127.0.0.1:080"}`, wantErr: "listen: port must be a decimal"},
		{name: "prefix not absolute", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "compat/e2b"}), wantErr: "compat_prefix: must be an absolute path"},
		{name: "prefix root", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/"}), wantErr: "compat_prefix: must not be the root path"},
		{name: "prefix empty segment", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat//e2b"}), wantErr: "compat_prefix: must not contain empty"},
		{name: "prefix dot segment", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat/./e2b"}), wantErr: "compat_prefix: must not contain empty"},
		{name: "prefix parent segment", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat/../e2b"}), wantErr: "compat_prefix: must not contain empty"},
		{name: "prefix query", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat?x=1"}), wantErr: "compat_prefix: must not contain a query"},
		{name: "prefix fragment", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat#f"}), wantErr: "compat_prefix: must not contain a query"},
		{name: "prefix percent encoding", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/compat%2Fe2b"}), wantErr: "compat_prefix: must not contain percent-encoding"},
		{name: "prefix is health path", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/healthz"}), wantErr: "compat_prefix: must not overlap the health check path"},
		{name: "prefix under health path", env: envWith(map[string]string{"E2B_SHIM_COMPAT_PREFIX": "/healthz/e2b"}), wantErr: "compat_prefix: must not overlap the health check path"},
		{name: "native url empty override", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": ""}), wantErr: "native_api_url: must not be empty"},
		{name: "native url bad scheme", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": "ftp://native.example.internal"}), wantErr: "native_api_url: scheme must be http or https"},
		{name: "native url user info", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": "https://user:pass@native.example.internal"}), wantErr: "native_api_url: must not contain user info"},
		{name: "native url query", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": "https://native.example.internal/v1?x=1"}), wantErr: "native_api_url: must not contain a query"},
		{name: "native url fragment", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": "https://native.example.internal/v1#f"}), wantErr: "native_api_url: must not contain a fragment"},
		{name: "native url no host", env: envWith(map[string]string{"E2B_SHIM_NATIVE_API_URL": "https://"}), wantErr: "native_api_url: must have a host"},
		{name: "empty adapter id", env: envWith(map[string]string{"E2B_SHIM_ADAPTER_ID": ""}), wantErr: "adapter_id: must not be empty"},
		{name: "empty hmac key version", env: envWith(map[string]string{"E2B_SHIM_HMAC_KEY_VERSION": ""}), wantErr: "hmac_key_version: must not be empty"},
		{name: "invalid domain", env: envWith(map[string]string{"E2B_SHIM_DOMAIN": "sbx/example"}), wantErr: "domain: must be a host name"},
		{name: "missing timeout max", file: strings.Replace(validFile, `  "timeout_max_seconds": 3600,`+"\n", "", 1), wantErr: "timeout_max_seconds: must be set explicitly"},
		{name: "timeout min above max", file: validFile, env: envWith(map[string]string{"E2B_SHIM_TIMEOUT_MIN_SECONDS": "7200"}), wantErr: "timeout_min_seconds: must not exceed timeout_max_seconds"},
		{name: "timeout default above max", file: validFile, env: envWith(map[string]string{"E2B_SHIM_TIMEOUT_DEFAULT_SECONDS": "7200"}), wantErr: "timeout_default_seconds: must not exceed timeout_max_seconds"},
		{name: "timeout default below min", file: validFile, env: envWith(map[string]string{"E2B_SHIM_TIMEOUT_DEFAULT_SECONDS": "30"}), wantErr: "timeout_default_seconds: must not be below timeout_min_seconds"},
		{name: "timeout min below one", file: validFile, env: envWith(map[string]string{"E2B_SHIM_TIMEOUT_MIN_SECONDS": "0"}), wantErr: "timeout_min_seconds: must be at least 1"},
		{name: "duplicate selector", file: `{"templates":[{"template_id":"a","native_template_id":"t1","revision":"r1","expected_envd_version":"0.8.0"},{"template_id":"a","native_template_id":"t2","revision":"r2","expected_envd_version":"0.8.0"}]}`, wantErr: "duplicate template selector"},
		{name: "duplicate alias", file: `{"templates":[{"template_id":"a","aliases":["x"],"native_template_id":"t1","revision":"r1","expected_envd_version":"0.8.0"},{"template_id":"b","aliases":["x"],"native_template_id":"t2","revision":"r2","expected_envd_version":"0.8.0"}]}`, wantErr: "duplicate template selector"},
		{name: "duplicate revision", file: `{"templates":[{"template_id":"a","native_template_id":"t1","revision":"r1","expected_envd_version":"0.8.0"},{"template_id":"b","native_template_id":"t2","revision":"r1","expected_envd_version":"0.8.0"}]}`, wantErr: "duplicate revision"},
		{name: "empty template field", file: `{"templates":[{"template_id":"a","native_template_id":"t","expected_envd_version":"0.8.0"}]}`, wantErr: "templates[0].revision: must not be empty"},
		{name: "empty alias", file: `{"templates":[{"template_id":"b","aliases":[""],"native_template_id":"t","revision":"r","expected_envd_version":"0.8.0"}]}`, wantErr: "templates[0].aliases[0]: must not be empty"},
		{name: "empty templates env", env: envWith(map[string]string{"E2B_SHIM_TEMPLATES": ""}), wantErr: "E2B_SHIM_TEMPLATES"},
		{name: "null templates env", env: envWith(map[string]string{"E2B_SHIM_TEMPLATES": "null"}), wantErr: "E2B_SHIM_TEMPLATES: must not be null"},
		{name: "null config file", file: " \nnull\t", wantErr: "config file: must not be null"},
		{name: "null template element in file", file: `{"templates": [null]}`, wantErr: "templates[0]: must not be null"},
		{name: "null template element in env", env: envWith(map[string]string{"E2B_SHIM_TEMPLATES": `[{}, null]`}), wantErr: "templates[1]: must not be null"},
		{name: "listen port out of range", file: `{"listen": "127.0.0.1:99999"}`, wantErr: "listen: port must be a decimal number", doNotEcho: "99999"},
		{name: "listen host whitespace", file: `{"listen": "foo bar:8080"}`, wantErr: "listen: host may contain only", doNotEcho: "foo bar"},
		{name: "listen host punctuation", file: `{"listen": "!!:8080"}`, wantErr: "listen: host may contain only"},
		{name: "prefix backslash", file: `{"compat_prefix": "/compat\\e2b"}`, wantErr: "compat_prefix: must not contain percent-encoding"},
		{name: "prefix whitespace", file: `{"compat_prefix": "/compat/e2 b"}`, wantErr: "compat_prefix: must not contain percent-encoding"},
		{name: "prefix control character", file: `{"compat_prefix": "/compat/e2b\u0000"}`, wantErr: "compat_prefix: must not contain percent-encoding"},
		{name: "native url parse error", file: `{"native_api_url": "https://user:pass@native.example/%zz"}`, wantErr: "native_api_url: is not a valid URL", doNotEcho: "native.example"},
		{name: "native url opaque", file: `{"native_api_url": "http:opaque"}`, wantErr: "native_api_url: must be a hierarchical URL"},
		{name: "native url force query", file: `{"native_api_url": "https://native.example/?"}`, wantErr: "native_api_url: must not contain a query"},
		{name: "native url port out of range", file: `{"native_api_url": "https://native.example:65536/v1"}`, wantErr: "native_api_url: port must be a number between 1 and 65535", doNotEcho: "65536"},
		{name: "native url port zero", file: `{"native_api_url": "https://native.example:0"}`, wantErr: "native_api_url: port must be a number between 1 and 65535"},
		{name: "native url empty explicit port", file: `{"native_api_url": "https://native.example:/v1"}`, wantErr: "native_api_url: port must be a number between 1 and 65535"},
		{name: "adapter id whitespace", env: envWith(map[string]string{"E2B_SHIM_ADAPTER_ID": " "}), wantErr: "adapter_id: must not be empty or contain whitespace"},
		{name: "adapter id control character", file: `{"adapter_id": "adapter\u0000"}`, wantErr: "adapter_id: must not be empty or contain whitespace"},
		{name: "hmac key version whitespace", env: envWith(map[string]string{"E2B_SHIM_HMAC_KEY_VERSION": "v1\n"}), wantErr: "hmac_key_version: must not be empty or contain whitespace"},
		{name: "hmac key version control character", file: `{"hmac_key_version": "v1\u0000"}`, wantErr: "hmac_key_version: must not be empty or contain whitespace"},
		{name: "domain contains port", file: `{"domain": "sandbox.example:443"}`, wantErr: "domain: must be a host name", doNotEcho: "sandbox.example"},
		{name: "domain control character", file: `{"domain": "sandbox.example\u0000"}`, wantErr: "domain: must be a host name"},
		{name: "timeout min overflows duration", file: `{"timeout_min_seconds": 9223372037}`, wantErr: "timeout_min_seconds: exceeds the maximum representable time.Duration", doNotEcho: "9223372037"},
		{name: "timeout default overflows duration", file: `{"timeout_default_seconds": 9223372037}`, wantErr: "timeout_default_seconds: exceeds the maximum representable time.Duration"},
		{name: "timeout max overflows duration", file: `{"timeout_max_seconds": 9223372037}`, wantErr: "timeout_max_seconds: exceeds the maximum representable time.Duration"},
		{name: "template id whitespace", file: strings.Replace(validFile, `"template_id": "base"`, `"template_id": "base "`, 1), wantErr: "templates[0].template_id: must not contain whitespace or control characters"},
		{name: "template alias control character", file: strings.Replace(validFile, `"base-envd"`, `"base-envd\u0000"`, 1), wantErr: "templates[0].aliases[0]: must not contain whitespace or control characters"},
		{name: "native template id whitespace", file: strings.Replace(validFile, `"tpl-base"`, `"tpl base"`, 1), wantErr: "templates[0].native_template_id: must not contain whitespace or control characters"},
		{name: "template revision control character", file: strings.Replace(validFile, `"tpl-base@rev-1"`, `"tpl-base@rev-1\u0000"`, 1), wantErr: "templates[0].revision: must not contain whitespace or control characters"},
		{name: "expected envd version whitespace", file: strings.Replace(validFile, `"0.8.0"`, `"0.8.0 "`, 1), wantErr: "templates[0].expected_envd_version: must not contain whitespace or control characters"},
		{name: "missing secrets", file: validFile, env: map[string]string{}, wantErr: "adapter_api_key: secret is not configured"},
		{name: "both secret sources", env: envWith(map[string]string{"E2B_SHIM_HMAC_KEY_FILE": unreadable}), wantErr: "both E2B_SHIM_HMAC_KEY and E2B_SHIM_HMAC_KEY_FILE are set"},
		{name: "empty secret env", env: envWith(map[string]string{"E2B_SHIM_ADAPTER_API_KEY": ""}), wantErr: "adapter_api_key: secret value is empty"},
		{name: "unreadable secret file", env: fileOverrideEnv("E2B_SHIM_NATIVE_API_KEY_FILE", filepath.Join(dir, "nope")), wantErr: "native_api_key: secret file is not readable"},
		{name: "empty secret file", env: fileOverrideEnv("E2B_SHIM_ADAPTER_API_KEY_FILE", emptyFile(t, dir)), wantErr: "adapter_api_key: secret value is empty"},
		{name: "reference with both", file: secretRefFile(`{"env": "A", "file": "/tmp/b"}`), env: hmacRefEnv(), wantErr: "reference must set exactly one of env or file"},
		{name: "reference with neither", file: secretRefFile(`{}`), env: hmacRefEnv(), wantErr: "reference must set exactly one of env or file"},
		{name: "referenced env unset", file: secretRefFile(`{"env": "NOT_SET_ANYWHERE"}`), env: hmacRefEnv(), wantErr: "referenced environment variable is not set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == nil {
				tc.env = secretEnv()
			}
			src := config.Source{Path: tc.file, LookupEnv: lookup(tc.env), Listen: tc.listen}
			if tc.file != "" && !filepath.IsAbs(tc.file) {
				src.Path = writeConfig(t, tc.file)
			}
			_, err := config.Load(src)
			if err == nil {
				t.Fatal("Load succeeded; want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantErr)
			}
			assertNoSecretLeak(t, err.Error())
			if tc.doNotEcho != "" && strings.Contains(err.Error(), tc.doNotEcho) {
				t.Errorf("error echoes an input value: %q", err)
			}
		})
	}
}

func fileOverrideEnv(name, path string) map[string]string {
	env := hmacRefEnv()
	switch name {
	case "E2B_SHIM_ADAPTER_API_KEY_FILE":
		delete(env, "E2B_SHIM_ADAPTER_API_KEY")
	case "E2B_SHIM_NATIVE_API_KEY_FILE":
		delete(env, "E2B_SHIM_NATIVE_API_KEY")
	}
	env[name] = path
	return env
}

// hmacRefEnv provides the adapter and native secrets directly while the HMAC
// key comes only from the file reference under test.
func hmacRefEnv() map[string]string {
	return map[string]string{
		"E2B_SHIM_ADAPTER_API_KEY": fakeAdapterKey,
		"E2B_SHIM_NATIVE_API_KEY":  fakeNativeKey,
	}
}

func secretRefFile(ref string) string {
	return `{"hmac_key": ` + ref + `, "native_api_url": "https://native.example.internal"}`
}

func emptyFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "empty-key")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ptr(s string) *string { return &s }

func assertNoSecretLeak(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{fakeAdapterKey, fakeNativeKey, fakeHMACKey, "user:pass"} {
		if strings.Contains(text, secret) {
			t.Errorf("text leaks %q", secret)
		}
	}
}

func TestConfigFormattingRedactsSecrets(t *testing.T) {
	cfg := mustLoad(t, config.Source{Path: writeConfig(t, validFile)}, nil)
	for _, text := range []string{
		cfg.AdapterAPIKey.LogValue().String(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%v", *cfg),
		fmt.Sprintf("%q", cfg.AdapterAPIKey),
		fmt.Sprintf("%v", &cfg.AdapterAPIKey),
		fmt.Sprintf("%#v", &cfg.AdapterAPIKey),
		fmt.Sprintf("%s %d %x", cfg.AdapterAPIKey, 1, cfg.HMACKey),
	} {
		assertNoSecretLeak(t, text)
		if !strings.Contains(text, "[REDACTED]") {
			t.Errorf("format output lacks redaction marker: %q", text)
		}
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecretLeak(t, string(blob))
	if !strings.Contains(string(blob), "[REDACTED]") {
		t.Error("JSON output lacks redaction marker")
	}
	_, err = config.Load(config.Source{LookupEnv: lookup(map[string]string{})})
	if err == nil {
		t.Fatal("Load without secrets succeeded; want error")
	}
	assertNoSecretLeak(t, err.Error())
}
