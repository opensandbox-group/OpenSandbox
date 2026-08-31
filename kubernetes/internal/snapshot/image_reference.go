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

package snapshot

import (
	"fmt"
	"strings"
)

// ImmutableImageReference replaces an optional tag with a registry manifest
// digest while preserving registry ports.
func ImmutableImageReference(imageURI, digest string) (string, error) {
	imageURI = strings.TrimSpace(imageURI)
	digest = strings.TrimSpace(digest)
	if imageURI == "" {
		return "", fmt.Errorf("image URI is required")
	}
	if _, err := ParseSHA256Digest(digest); err != nil {
		return "", err
	}

	if at := strings.IndexByte(imageURI, '@'); at >= 0 {
		imageURI = imageURI[:at]
	}
	lastSlash := strings.LastIndexByte(imageURI, '/')
	if colon := strings.LastIndexByte(imageURI, ':'); colon > lastSlash {
		imageURI = imageURI[:colon]
	}
	if imageURI == "" {
		return "", fmt.Errorf("invalid image URI")
	}
	return imageURI + "@" + digest, nil
}

// ParseSHA256Digest validates a canonical OCI sha256 digest and returns its
// lowercase hexadecimal portion.
func ParseSHA256Digest(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	for _, ch := range hex {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return "", fmt.Errorf("invalid sha256 digest %q", digest)
		}
	}
	return hex, nil
}
