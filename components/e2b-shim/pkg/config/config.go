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

// Package config loads and validates e2b-shim configuration.
//
// Sources, highest precedence first: explicit --listen flag, E2B_SHIM_*
// environment variables, the JSON file, defaults. A set environment variable
// always wins, even when empty, so an empty override fails validation rather
// than silently falling back to the file. Error messages name fields and
// failure classes only; they never echo values, raw configuration or URLs.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// HealthPath is the only management path served by the scaffold.
const HealthPath = "/healthz"

const (
	DefaultListen                = "127.0.0.1:8080"
	DefaultCompatPrefix          = "/compat/e2b"
	DefaultTimeoutDefaultSeconds = 300
	DefaultTimeoutMinSeconds     = 60
)

// Config is the validated, resolved configuration. Secret fields are exported
// so fmt's %v/%+v/%#v reach Secret's redacting Formatter.
type Config struct {
	Listen                string            `json:"listen"`
	CompatPrefix          string            `json:"compat_prefix"`
	NativeAPIURL          string            `json:"native_api_url"`
	AdapterID             string            `json:"adapter_id"`
	Domain                string            `json:"domain"`
	HMACKeyVersion        string            `json:"hmac_key_version"`
	TimeoutDefaultSeconds int64             `json:"timeout_default_seconds"`
	TimeoutMinSeconds     int64             `json:"timeout_min_seconds"`
	TimeoutMaxSeconds     int64             `json:"timeout_max_seconds"`
	Templates             []TemplateMapping `json:"templates"`
	AdapterAPIKey         Secret            `json:"adapter_api_key"`
	NativeAPIKey          Secret            `json:"native_api_key"`
	HMACKey               Secret            `json:"hmac_key"`
}

// TemplateMapping maps an E2B template ID and aliases to an immutable native
// template revision. It holds configuration only; readiness is not checked.
type TemplateMapping struct {
	TemplateID          string   `json:"template_id"`
	Aliases             []string `json:"aliases,omitempty"`
	NativeTemplateID    string   `json:"native_template_id"`
	Revision            string   `json:"revision"`
	ExpectedEnvdVersion string   `json:"expected_envd_version"`
}

// Source describes where configuration is read from.
type Source struct {
	// Path is the JSON file; empty means environment and defaults only.
	Path string
	// LookupEnv defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Listen, when non-nil, is an explicit --listen value (may be empty).
	Listen *string
}

type stringField struct {
	key, env string
	dst      func(*Config) *string
}

type intField struct {
	key, env string
	dst      func(*Config) *int64
}

var stringFields = []stringField{
	{"listen", "E2B_SHIM_LISTEN", func(c *Config) *string { return &c.Listen }},
	{"compat_prefix", "E2B_SHIM_COMPAT_PREFIX", func(c *Config) *string { return &c.CompatPrefix }},
	{"native_api_url", "E2B_SHIM_NATIVE_API_URL", func(c *Config) *string { return &c.NativeAPIURL }},
	{"adapter_id", "E2B_SHIM_ADAPTER_ID", func(c *Config) *string { return &c.AdapterID }},
	{"domain", "E2B_SHIM_DOMAIN", func(c *Config) *string { return &c.Domain }},
	{"hmac_key_version", "E2B_SHIM_HMAC_KEY_VERSION", func(c *Config) *string { return &c.HMACKeyVersion }},
}

var intFields = []intField{
	{"timeout_default_seconds", "E2B_SHIM_TIMEOUT_DEFAULT_SECONDS", func(c *Config) *int64 { return &c.TimeoutDefaultSeconds }},
	{"timeout_min_seconds", "E2B_SHIM_TIMEOUT_MIN_SECONDS", func(c *Config) *int64 { return &c.TimeoutMinSeconds }},
	{"timeout_max_seconds", "E2B_SHIM_TIMEOUT_MAX_SECONDS", func(c *Config) *int64 { return &c.TimeoutMaxSeconds }},
}

const (
	templatesKey = "templates"
	templatesEnv = "E2B_SHIM_TEMPLATES"
)

var secretSpecs = []struct {
	secretSpec
	dst func(*Config) *Secret
}{
	{secretSpec{"adapter_api_key", "E2B_SHIM_ADAPTER_API_KEY"}, func(c *Config) *Secret { return &c.AdapterAPIKey }},
	{secretSpec{"native_api_key", "E2B_SHIM_NATIVE_API_KEY"}, func(c *Config) *Secret { return &c.NativeAPIKey }},
	{secretSpec{"hmac_key", "E2B_SHIM_HMAC_KEY"}, func(c *Config) *Secret { return &c.HMACKey }},
}

func knownKeys() map[string]bool {
	keys := map[string]bool{templatesKey: true}
	for _, f := range stringFields {
		keys[f.key] = true
	}
	for _, f := range intFields {
		keys[f.key] = true
	}
	for _, s := range secretSpecs {
		keys[s.field] = true
	}
	return keys
}

// Load reads all sources, resolves secrets and validates the result. No
// network access happens here.
func Load(src Source) (*Config, error) {
	lookupEnv := src.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	raw := map[string]json.RawMessage{}
	if src.Path != "" {
		data, err := os.ReadFile(src.Path)
		if err != nil {
			return nil, errors.New("config file: is not readable")
		}
		if err := decodeStrict("config file", data, &raw); err != nil {
			return nil, err
		}
		known := knownKeys()
		for k := range raw {
			if !known[k] {
				return nil, fmt.Errorf("config file: unknown field %q", k)
			}
		}
	}

	cfg := &Config{
		Listen:                DefaultListen,
		CompatPrefix:          DefaultCompatPrefix,
		TimeoutDefaultSeconds: DefaultTimeoutDefaultSeconds,
		TimeoutMinSeconds:     DefaultTimeoutMinSeconds,
		Templates:             []TemplateMapping{},
	}
	var errs []error
	for _, f := range stringFields {
		// The --listen flag outranks both lower-priority sources, so neither
		// is decoded when it is set.
		if f.key == "listen" && src.Listen != nil {
			cfg.Listen = *src.Listen
		} else if v, ok := lookupEnv(f.env); ok {
			*f.dst(cfg) = v
		} else if err := decodeField(raw, f.key, f.dst(cfg)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range intFields {
		if v, ok := lookupEnv(f.env); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s is not a base-10 integer", f.key, f.env))
				continue
			}
			*f.dst(cfg) = n
		} else if err := decodeField(raw, f.key, f.dst(cfg)); err != nil {
			errs = append(errs, err)
		}
	}
	if v, ok := lookupEnv(templatesEnv); ok {
		if err := decodeTemplates(templatesKey+": "+templatesEnv, []byte(v), &cfg.Templates); err != nil {
			errs = append(errs, err)
		}
	} else if msg, ok := raw[templatesKey]; ok {
		if err := decodeTemplates(templatesKey, msg, &cfg.Templates); err != nil {
			errs = append(errs, err)
		}
	}
	for _, s := range secretSpecs {
		var ref *SecretRef
		if msg, ok := raw[s.field]; ok {
			ref = &SecretRef{}
			if err := decodeStrict(s.field, msg, ref); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		secret, err := resolveSecret(s.secretSpec, ref, lookupEnv)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*s.dst(cfg) = secret
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// decodeField decodes raw[key] into dst when present; null is rejected.
func decodeField(raw map[string]json.RawMessage, key string, dst any) error {
	msg, ok := raw[key]
	if !ok {
		return nil
	}
	return decodeStrict(key, msg, dst)
}

func decodeStrict(key string, msg json.RawMessage, dst any) error {
	if bytes.Equal(bytes.TrimSpace(msg), []byte("null")) {
		return fmt.Errorf("%s: must not be null", key)
	}
	if err := strictUnmarshal(msg, dst); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func decodeTemplates(field string, data []byte, dst *[]TemplateMapping) error {
	var entries []*TemplateMapping
	if err := decodeStrict(field, data, &entries); err != nil {
		return err
	}
	out := make([]TemplateMapping, len(entries))
	for i, entry := range entries {
		if entry == nil {
			return fmt.Errorf("%s[%d]: must not be null", templatesKey, i)
		}
		out[i] = *entry
	}
	*dst = out
	return nil
}

// strictUnmarshal rejects unknown fields and trailing data. Returned errors
// carry a position or field name, never the offending input.
func strictUnmarshal(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return sanitizeJSONError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON value")
	}
	return nil
}

func sanitizeJSONError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		return fmt.Errorf("invalid JSON at offset %d", syntaxErr.Offset)
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return fmt.Errorf("field %q has the wrong type, want %s", typeErr.Field, typeErr.Type)
		}
		return fmt.Errorf("wrong type, want %s", typeErr.Type)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("incomplete JSON")
	default:
		// The remaining decoder error is "json: unknown field \"name\"", which
		// names a key rather than a value.
		return err
	}
}
