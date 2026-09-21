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
	"testing"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetEndpoints(t *testing.T) {
	tests := []struct {
		name          string
		bs            *sandboxv1alpha1.BatchSandbox
		expectedIPs   []string
		expectedError string
	}{
		{
			name:          "nil BatchSandbox",
			bs:            nil,
			expectedIPs:   nil,
			expectedError: "BatchSandbox is nil",
		},
		{
			name: "no annotations",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			expectedIPs:   nil,
			expectedError: "has no annotations",
		},
		{
			name: "missing endpoints annotation",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						"other-key": "other-value",
					},
				},
			},
			expectedIPs:   nil,
			expectedError: "missing sandbox.opensandbox.io/endpoints annotation",
		},
		{
			name: "invalid JSON annotation",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationEndpoints: "invalid-json",
					},
				},
			},
			expectedIPs:   nil,
			expectedError: "failed to parse endpoints annotation",
		},
		{
			name: "empty endpoints array",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationEndpoints: "[]",
					},
				},
			},
			expectedIPs:   nil,
			expectedError: "contains no IPs",
		},
		{
			name: "single endpoint",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationEndpoints: `["10.244.1.5"]`,
					},
				},
			},
			expectedIPs:   []string{"10.244.1.5"},
			expectedError: "",
		},
		{
			name: "multiple endpoints",
			bs: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationEndpoints: `["10.244.1.5", "10.244.1.6", "10.244.1.7"]`,
					},
				},
			},
			expectedIPs:   []string{"10.244.1.5", "10.244.1.6", "10.244.1.7"},
			expectedError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips, err := GetEndpoints(tt.bs)

			if tt.expectedError != "" {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.expectedError)
					return
				}
				if err.Error() == "" || !contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if len(ips) != len(tt.expectedIPs) {
				t.Errorf("expected %d IPs, got %d", len(tt.expectedIPs), len(ips))
				return
			}

			for i, ip := range ips {
				if ip != tt.expectedIPs[i] {
					t.Errorf("expected IP[%d]=%s, got %s", i, tt.expectedIPs[i], ip)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			len(s) > len(substr) && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
