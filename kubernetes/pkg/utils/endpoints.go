// Copyright 2025 The OpenSandbox Authors
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

package utils

import (
	"encoding/json"
	"fmt"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (
	// AnnotationEndpoints is the annotation key for storing BatchSandbox endpoints
	AnnotationEndpoints = "sandbox.opensandbox.io/endpoints"
)

// GetEndpoints extracts endpoint IPs from BatchSandbox annotations
// Returns a slice of IP addresses parsed from the endpoints annotation
// The annotation format is a JSON array: ["10.244.1.5", "10.244.1.6"]
func GetEndpoints(bs *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	if bs == nil {
		return nil, fmt.Errorf("BatchSandbox is nil")
	}

	if bs.Annotations == nil {
		return nil, fmt.Errorf("BatchSandbox has no annotations")
	}

	endpointsAnnotation := bs.Annotations[AnnotationEndpoints]
	if endpointsAnnotation == "" {
		return nil, fmt.Errorf("missing %s annotation", AnnotationEndpoints)
	}

	var endpoints []string
	if err := json.Unmarshal([]byte(endpointsAnnotation), &endpoints); err != nil {
		return nil, fmt.Errorf("failed to parse endpoints annotation: %w", err)
	}

	if len(endpoints) == 0 {
		return nil, fmt.Errorf("endpoints annotation contains no IPs")
	}

	return endpoints, nil
}
