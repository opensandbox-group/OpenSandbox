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
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Validate checks every field and normalizes CompatPrefix (one trailing
// slash removed) and NativeAPIURL (trailing slashes removed). All problems
// are reported together.
func (c *Config) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(validateListen(c.Listen))
	prefix, err := normalizePrefix(c.CompatPrefix)
	add(err)
	if err == nil {
		c.CompatPrefix = prefix
	}
	nativeURL, err := normalizeNativeURL(c.NativeAPIURL)
	add(err)
	if err == nil {
		c.NativeAPIURL = nativeURL
	}
	if c.AdapterID == "" || hasWhitespaceOrControl(c.AdapterID) {
		add(errors.New("adapter_id: must not be empty or contain whitespace or control characters"))
	}
	if c.HMACKeyVersion == "" || hasWhitespaceOrControl(c.HMACKeyVersion) {
		add(errors.New("hmac_key_version: must not be empty or contain whitespace or control characters"))
	}
	if strings.ContainsAny(c.Domain, "/?#@:") || hasWhitespaceOrControl(c.Domain) {
		add(errors.New("domain: must be a host name without scheme, path, query, fragment, user info, port, whitespace or control characters"))
	}
	add(c.validateTimeouts())
	add(validateTemplates(c.Templates))
	for _, s := range []struct {
		name   string
		secret Secret
	}{{"adapter_api_key", c.AdapterAPIKey}, {"native_api_key", c.NativeAPIKey}, {"hmac_key", c.HMACKey}} {
		if s.secret.IsZero() {
			add(fmt.Errorf("%s: %w", s.name, errSecretMissing))
		}
	}
	return errors.Join(errs...)
}

func validateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("listen: must be host:port")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || strconv.FormatUint(n, 10) != port {
		return errors.New("listen: port must be a decimal number between 0 and 65535")
	}
	if strings.ContainsFunc(host, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".-_:[]", r)
	}) {
		return errors.New("listen: host may contain only letters, digits and .-_:[]")
	}
	return nil
}

func hasWhitespaceOrControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// normalizePrefix accepts an absolute, unambiguous path below the root and
// removes a single trailing slash.
func normalizePrefix(p string) (string, error) {
	const field = "compat_prefix"
	if !strings.HasPrefix(p, "/") {
		return "", errors.New(field + ": must be an absolute path")
	}
	if strings.ContainsAny(p, "?#") {
		return "", errors.New(field + ": must not contain a query or fragment")
	}
	if strings.ContainsAny(p, "%\\") || hasWhitespaceOrControl(p) {
		return "", errors.New(field + ": must not contain percent-encoding, backslashes, whitespace or control characters")
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", errors.New(field + ": must not be the root path")
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New(field + ": must not contain empty, '.' or '..' segments")
		}
	}
	if p == HealthPath || strings.HasPrefix(p, HealthPath+"/") {
		return "", errors.New(field + ": must not overlap the health check path")
	}
	return p, nil
}

func normalizeNativeURL(raw string) (string, error) {
	const field = "native_api_url"
	if raw == "" {
		return "", errors.New(field + ": must not be empty")
	}
	// url.Parse errors quote the input, which may hold credentials, so they
	// are replaced by a fixed message.
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New(field + ": is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New(field + ": scheme must be http or https")
	case u.Opaque != "":
		return "", errors.New(field + ": must be a hierarchical URL")
	case u.User != nil:
		return "", errors.New(field + ": must not contain user info")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New(field + ": must have a host")
	case u.RawQuery != "" || u.ForceQuery:
		return "", errors.New(field + ": must not contain a query")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", errors.New(field + ": must not contain a fragment")
	}
	// Port returns an empty string for both an absent port and an explicit empty port.
	if port := u.Port(); port != "" || strings.HasSuffix(u.Host, ":") {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return "", errors.New(field + ": port must be a number between 1 and 65535")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *Config) validateTimeouts() error {
	var errs []error
	if c.TimeoutMaxSeconds == 0 {
		errs = append(errs, errors.New("timeout_max_seconds: must be set explicitly to a positive integer"))
	}
	if c.TimeoutMinSeconds < 1 {
		errs = append(errs, errors.New("timeout_min_seconds: must be at least 1"))
	}
	if c.TimeoutDefaultSeconds < c.TimeoutMinSeconds {
		errs = append(errs, errors.New("timeout_default_seconds: must not be below timeout_min_seconds"))
	}
	if c.TimeoutMaxSeconds != 0 && c.TimeoutDefaultSeconds > c.TimeoutMaxSeconds {
		errs = append(errs, errors.New("timeout_default_seconds: must not exceed timeout_max_seconds"))
	}
	if c.TimeoutMaxSeconds != 0 && c.TimeoutMinSeconds > c.TimeoutMaxSeconds {
		errs = append(errs, errors.New("timeout_min_seconds: must not exceed timeout_max_seconds"))
	}
	const maxDurationSeconds = math.MaxInt64 / int64(time.Second)
	for _, f := range []struct {
		name    string
		seconds int64
	}{
		{"timeout_min_seconds", c.TimeoutMinSeconds},
		{"timeout_default_seconds", c.TimeoutDefaultSeconds},
		{"timeout_max_seconds", c.TimeoutMaxSeconds},
	} {
		if f.seconds > maxDurationSeconds {
			errs = append(errs, fmt.Errorf("%s: exceeds the maximum representable time.Duration in seconds", f.name))
		}
	}
	return errors.Join(errs...)
}

// validateTemplates requires every field and makes template IDs and aliases a
// single unique selector namespace; revisions must be unique so a stored
// revision reference resolves to exactly one mapping.
func validateTemplates(ts []TemplateMapping) error {
	var errs []error
	selectors := map[string]bool{}
	revisions := map[string]bool{}
	for i, t := range ts {
		at := fmt.Sprintf("templates[%d]", i)
		for _, f := range []struct{ name, v string }{
			{"template_id", t.TemplateID},
			{"native_template_id", t.NativeTemplateID},
			{"revision", t.Revision},
			{"expected_envd_version", t.ExpectedEnvdVersion},
		} {
			if f.v == "" {
				errs = append(errs, fmt.Errorf("%s.%s: must not be empty", at, f.name))
			} else if hasWhitespaceOrControl(f.v) {
				errs = append(errs, fmt.Errorf("%s.%s: must not contain whitespace or control characters", at, f.name))
			}
		}
		for j, sel := range append([]string{t.TemplateID}, t.Aliases...) {
			if sel == "" {
				if j > 0 {
					errs = append(errs, fmt.Errorf("%s.aliases[%d]: must not be empty", at, j-1))
				}
				continue
			}
			if j > 0 && hasWhitespaceOrControl(sel) {
				errs = append(errs, fmt.Errorf("%s.aliases[%d]: must not contain whitespace or control characters", at, j-1))
			}
			if selectors[sel] {
				errs = append(errs, fmt.Errorf("%s: duplicate template selector", at))
			}
			selectors[sel] = true
		}
		if t.Revision != "" {
			if revisions[t.Revision] {
				errs = append(errs, fmt.Errorf("%s.revision: duplicate revision", at))
			}
			revisions[t.Revision] = true
		}
	}
	return errors.Join(errs...)
}
