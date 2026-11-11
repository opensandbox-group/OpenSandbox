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

// Package identity derives stable cryptographic identities for storage
// targets, finalize intents, and canonical OSS endpoints. Identities bind
// persisted state to a specific target configuration so a misconfigured
// restart cannot mix or silently adopt foreign data.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OSSTargetID returns the durable state identity of the OSS target defined by
// endpoint, bucket, key prefix, and cluster. All fields participate in the
// digest; a change in any of them is a different target.
func OSSTargetID(endpoint, bucket, prefix, clusterID string) (string, error) {
	normalized, err := CanonicalOSSEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	if bucket == "" || strings.Trim(prefix, "/") == "" || clusterID == "" {
		return "", errors.New("OSS target identity fields are required")
	}
	return digest("opensandbox-nodeagent-target-v1\x00", "oss", normalized, bucket, strings.Trim(prefix, "/"), clusterID), nil
}

// CanonicalOSSEndpoint normalizes an OSS endpoint to its HTTPS origin form
// (lowercased host, default port elided, IPv6 hosts bracketed). Anything but
// a plain HTTPS origin is rejected.
func CanonicalOSSEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("OSS endpoint must be an HTTPS origin")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("OSS endpoint must be an HTTPS origin")
	}
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}

// FileTargetID returns the durable state identity of the file target at root,
// canonicalized through symlink resolution so the same physical directory
// reached by different paths maps to one identity.
func FileTargetID(root, clusterID, nodeName string) (string, error) {
	canonical, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	canonical, err = resolveExistingAncestor(canonical)
	if err != nil {
		return "", err
	}
	if canonical == string(filepath.Separator) || clusterID == "" || nodeName == "" {
		return "", errors.New("file target identity fields are invalid")
	}
	return digest("opensandbox-nodeagent-target-v1\x00", "file", canonical, clusterID, nodeName), nil
}

func resolveExistingAncestor(path string) (string, error) {
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// StdoutTargetID returns the durable state identity of the stdout-only mode,
// which has no configurable location of its own.
func StdoutTargetID(clusterID, nodeName string) string {
	return digest("opensandbox-nodeagent-target-v1\x00", "stdout", clusterID, nodeName)
}

// FinalizeID derives the idempotent identity of one stream revision's
// finalization for a target. Sinks use it to deduplicate finalization work
// across retries and restarts.
func FinalizeID(streamRef string, revision uint64, targetID string) string {
	return digest("opensandbox-nodeagent-finalize-v1\x00", streamRef, strconv.FormatUint(revision, 10), targetID)
}

// digest joins domain-separated, length-prefixed parts into a hex-encoded
// SHA-256 digest.
func digest(domain string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len([]byte(part)), part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
