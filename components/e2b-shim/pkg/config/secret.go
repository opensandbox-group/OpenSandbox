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

package config

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

const redacted = "[REDACTED]"

// Secret holds a resolved secret value. Every formatting path (fmt verbs,
// JSON, text marshaling, slog) prints a fixed placeholder instead of the value.
type Secret struct {
	value string
}

// IsZero reports whether no value is held.
func (s Secret) IsZero() bool { return s.value == "" }

// Reveal returns the raw value. Callers must not log or echo it.
func (s Secret) Reveal() string { return s.value }

// Equal rejects an empty secret, otherwise comparing with candidate in constant time.
func (s Secret) Equal(candidate string) bool {
	return !s.IsZero() && subtle.ConstantTimeCompare([]byte(s.value), []byte(candidate)) == 1
}

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format redacts the value for every fmt verb, including %x and %q.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }

// SecretRef names where a secret is read from. Exactly one field is set.
// The configuration file only accepts references, never literal values.
type SecretRef struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// secretSpec ties a configuration field to its direct environment overrides.
type secretSpec struct {
	field   string // JSON field name, used in error messages
	envName string // E2B_SHIM_X; E2B_SHIM_X_FILE is derived
}

var (
	errSecretMissing = errors.New("secret is not configured")
	errSecretEmpty   = errors.New("secret value is empty")
)

// resolveSecret applies the precedence: direct environment override
// (E2B_SHIM_X or E2B_SHIM_X_FILE, never both), then the file reference.
// Errors name the field and the failure class only.
func resolveSecret(spec secretSpec, ref *SecretRef, lookupEnv func(string) (string, bool)) (Secret, error) {
	value, valueSet := lookupEnv(spec.envName)
	path, pathSet := lookupEnv(spec.envName + "_FILE")
	switch {
	case valueSet && pathSet:
		return Secret{}, fmt.Errorf("%s: both %s and %s_FILE are set", spec.field, spec.envName, spec.envName)
	case valueSet:
		return secretFromValue(spec.field, value)
	case pathSet:
		return secretFromFile(spec.field, path)
	case ref == nil:
		return Secret{}, fmt.Errorf("%s: %w", spec.field, errSecretMissing)
	case (ref.Env == "") == (ref.File == ""):
		return Secret{}, fmt.Errorf("%s: reference must set exactly one of env or file", spec.field)
	case ref.Env != "":
		v, ok := lookupEnv(ref.Env)
		if !ok {
			return Secret{}, fmt.Errorf("%s: referenced environment variable is not set", spec.field)
		}
		return secretFromValue(spec.field, v)
	default:
		return secretFromFile(spec.field, ref.File)
	}
}

func secretFromValue(field, v string) (Secret, error) {
	if v == "" {
		return Secret{}, fmt.Errorf("%s: %w", field, errSecretEmpty)
	}
	return Secret{value: v}, nil
}

// secretFromFile strips one trailing "\n" or "\r\n" (as written by editors and
// `echo`) and nothing else, so leading/inner whitespace keeps its meaning.
func secretFromFile(field, path string) (Secret, error) {
	if path == "" {
		return Secret{}, fmt.Errorf("%s: secret file path is empty", field)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied secret path
	if err != nil {
		return Secret{}, fmt.Errorf("%s: secret file is not readable", field)
	}
	v := string(data)
	if s, ok := strings.CutSuffix(v, "\r\n"); ok {
		v = s
	} else {
		v = strings.TrimSuffix(v, "\n")
	}
	return secretFromValue(field, v)
}
