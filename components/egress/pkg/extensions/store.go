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

package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	ProtocolVersion = "extensions.opensandbox.io/v1alpha1"

	CodeInvalidExtension      = "INVALID_EXTENSION"
	CodeUnsupportedExtension  = "UNSUPPORTED_EXTENSION"
	CodeCapabilityUnavailable = "CAPABILITY_UNAVAILABLE"
	CodeExtensionConflict     = "EXTENSION_CONFLICT"
	CodeRevisionConflict      = "REVISION_CONFLICT"
	CodeApplyFailed           = "EXTENSION_APPLY_FAILED"

	maxRequestBodyBytes = 1 << 20
	maxResources        = 256
)

type Capability struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Available  bool     `json:"available"`
	Operations []string `json:"operations"`
	Features   []string `json:"features,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

type CapabilitiesResponse struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Resources       []Capability `json:"resources"`
}

type Resource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

type ResourceReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type Condition struct {
	Type             string             `json:"type"`
	Status           string             `json:"status"`
	Reason           string             `json:"reason"`
	Message          string             `json:"message,omitempty"`
	ObservedRevision uint64             `json:"observedRevision"`
	Resource         *ResourceReference `json:"resource,omitempty"`
}

type State struct {
	Revision   uint64      `json:"revision"`
	Resources  []Resource  `json:"resources"`
	Conditions []Condition `json:"conditions"`
}

type ReplaceRequest struct {
	ExpectedRevision *uint64    `json:"expectedRevision,omitempty"`
	Resources        []Resource `json:"resources"`
}

type ApplyResult struct {
	ProgrammedStatus string
	Reason           string
	Message          string
}

type Applier interface {
	Apply(context.Context, uint64, []Resource) (ApplyResult, error)
}

type POCApplier struct{}

func (POCApplier) Apply(_ context.Context, _ uint64, _ []Resource) (ApplyResult, error) {
	return ApplyResult{
		ProgrammedStatus: "Unknown",
		Reason:           "PocStored",
		Message:          "resource set was validated and stored but is not enforced",
	}, nil
}

type APIError struct {
	Status    int                `json:"-"`
	Code      string             `json:"code"`
	Message   string             `json:"message"`
	Resource  *ResourceReference `json:"resource,omitempty"`
	FieldPath string             `json:"fieldPath,omitempty"`
}

func (e *APIError) Error() string {
	return e.Message
}

type Store struct {
	mu      sync.Mutex
	applier Applier
	state   State
}

type resourceKey struct {
	apiVersion string
	kind       string
}

var pocCapabilities = map[resourceKey]Capability{
	{apiVersion: "gateway.networking.k8s.io/v1", kind: "Gateway"}: {
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       "Gateway",
		Features:   []string{"extensions.opensandbox.io/StorageOnlyPOC"},
	},
	{apiVersion: "gateway.networking.k8s.io/v1", kind: "HTTPRoute"}: {
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       "HTTPRoute",
		Features:   []string{"extensions.opensandbox.io/StorageOnlyPOC"},
	},
	{apiVersion: "agentgateway.dev/v1alpha1", kind: "AgentgatewayBackend"}: {
		APIVersion: "agentgateway.dev/v1alpha1",
		Kind:       "AgentgatewayBackend",
		Features:   []string{"extensions.opensandbox.io/StorageOnlyPOC"},
	},
}

var unavailableCapabilities = map[resourceKey]Capability{
	{apiVersion: "gateway.networking.k8s.io/v1alpha2", kind: "TLSRoute"}: {
		APIVersion: "gateway.networking.k8s.io/v1alpha2",
		Kind:       "TLSRoute",
		Reason:     "TLSRoute translation is not implemented by the POC",
	},
	{apiVersion: "agentgateway.dev/v1alpha1", kind: "AgentgatewayPolicy"}: {
		APIVersion: "agentgateway.dev/v1alpha1",
		Kind:       "AgentgatewayPolicy",
		Reason:     "AgentgatewayPolicy translation is not implemented by the POC",
	},
}

func NewStore(applier Applier) *Store {
	return &Store{
		applier: applier,
		state: State{
			Resources:  []Resource{},
			Conditions: []Condition{},
		},
	}
}

func (s *Store) Capabilities() CapabilitiesResponse {
	resources := make([]Capability, 0, len(pocCapabilities)+len(unavailableCapabilities))
	for _, capability := range pocCapabilities {
		capability.Available = s.applier != nil
		if capability.Available {
			capability.Operations = []string{"read", "replace"}
		} else {
			capability.Operations = []string{}
			capability.Reason = "extension applier is not configured"
		}
		resources = append(resources, capability)
	}
	for _, capability := range unavailableCapabilities {
		capability.Operations = []string{}
		resources = append(resources, capability)
	}
	sortCapabilities(resources)
	return CapabilitiesResponse{ProtocolVersion: ProtocolVersion, Resources: resources}
}

func (s *Store) Current() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func (s *Store) Replace(ctx context.Context, req ReplaceRequest) (State, *APIError) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.ExpectedRevision != nil && *req.ExpectedRevision != s.state.Revision {
		return State{}, &APIError{
			Status:  http.StatusConflict,
			Code:    CodeRevisionConflict,
			Message: fmt.Sprintf("expected revision %d, current revision is %d", *req.ExpectedRevision, s.state.Revision),
		}
	}
	if apiErr := s.validateResources(req.Resources); apiErr != nil {
		return State{}, apiErr
	}

	resources, err := cloneResources(req.Resources)
	if err != nil {
		return State{}, &APIError{Status: http.StatusBadRequest, Code: CodeInvalidExtension, Message: err.Error()}
	}
	nextRevision := s.state.Revision + 1
	result := ApplyResult{ProgrammedStatus: "True", Reason: "Acknowledged"}
	if s.applier == nil {
		return State{}, &APIError{
			Status:  http.StatusPreconditionFailed,
			Code:    CodeCapabilityUnavailable,
			Message: "extension replacement is unavailable because no applier is configured",
		}
	} else {
		applyResources, cloneErr := cloneResources(resources)
		if cloneErr != nil {
			return State{}, &APIError{Status: http.StatusBadRequest, Code: CodeInvalidExtension, Message: cloneErr.Error()}
		}
		result, err = s.applier.Apply(ctx, nextRevision, applyResources)
		if err != nil {
			return State{}, &APIError{
				Status:  http.StatusInternalServerError,
				Code:    CodeApplyFailed,
				Message: fmt.Sprintf("failed to apply extension revision %d: %v", nextRevision, err),
			}
		}
	}
	if result.ProgrammedStatus != "True" && result.ProgrammedStatus != "False" && result.ProgrammedStatus != "Unknown" {
		return State{}, &APIError{
			Status:  http.StatusInternalServerError,
			Code:    CodeApplyFailed,
			Message: fmt.Sprintf("applier returned invalid programmed status %q", result.ProgrammedStatus),
		}
	}

	s.state = State{
		Revision:  nextRevision,
		Resources: resources,
		Conditions: []Condition{
			{Type: "Accepted", Status: "True", Reason: "Valid", ObservedRevision: nextRevision},
			{
				Type:             "Programmed",
				Status:           result.ProgrammedStatus,
				Reason:           result.Reason,
				Message:          result.Message,
				ObservedRevision: nextRevision,
			},
		},
	}
	return cloneState(s.state), nil
}

func (s *Store) validateResources(resources []Resource) *APIError {
	if len(resources) > maxResources {
		return &APIError{
			Status:  http.StatusBadRequest,
			Code:    CodeInvalidExtension,
			Message: fmt.Sprintf("resource count %d exceeds limit %d", len(resources), maxResources),
		}
	}
	seen := make(map[string]struct{}, len(resources))
	for index, resource := range resources {
		ref, apiErr := validateResource(resource, index)
		if apiErr != nil {
			return apiErr
		}
		key := resourceKey{apiVersion: resource.APIVersion, kind: resource.Kind}
		if _, ok := pocCapabilities[key]; !ok {
			if capability, known := unavailableCapabilities[key]; known {
				return &APIError{
					Status:   http.StatusPreconditionFailed,
					Code:     CodeCapabilityUnavailable,
					Message:  capability.Reason,
					Resource: &ref,
				}
			}
			return &APIError{
				Status:   http.StatusUnprocessableEntity,
				Code:     CodeUnsupportedExtension,
				Message:  fmt.Sprintf("unsupported extension %s %s", resource.APIVersion, resource.Kind),
				Resource: &ref,
			}
		}
		if s.applier == nil {
			return &APIError{
				Status:   http.StatusPreconditionFailed,
				Code:     CodeCapabilityUnavailable,
				Message:  "extension applier is not configured",
				Resource: &ref,
			}
		}
		identity := strings.Join([]string{ref.APIVersion, ref.Kind, ref.Namespace, ref.Name}, "\x00")
		if _, exists := seen[identity]; exists {
			return &APIError{
				Status:    http.StatusConflict,
				Code:      CodeExtensionConflict,
				Message:   "duplicate extension resource identity",
				Resource:  &ref,
				FieldPath: fmt.Sprintf("resources[%d]", index),
			}
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func validateResource(resource Resource, index int) (ResourceReference, *APIError) {
	ref := ResourceReference{APIVersion: resource.APIVersion, Kind: resource.Kind}
	fieldPrefix := fmt.Sprintf("resources[%d]", index)
	if strings.TrimSpace(resource.APIVersion) == "" || !strings.Contains(resource.APIVersion, "/") {
		return ref, invalidField(ref, fieldPrefix+".apiVersion", "apiVersion must be a qualified group/version")
	}
	if strings.TrimSpace(resource.Kind) == "" {
		return ref, invalidField(ref, fieldPrefix+".kind", "kind is required")
	}
	if resource.Metadata == nil {
		return ref, invalidField(ref, fieldPrefix+".metadata", "metadata is required")
	}
	name, ok := resource.Metadata["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return ref, invalidField(ref, fieldPrefix+".metadata.name", "metadata.name must be a non-empty string")
	}
	ref.Name = name
	if namespace, exists := resource.Metadata["namespace"]; exists {
		value, ok := namespace.(string)
		if !ok {
			return ref, invalidField(ref, fieldPrefix+".metadata.namespace", "metadata.namespace must be a string")
		}
		ref.Namespace = value
	}
	if resource.Spec == nil {
		return ref, invalidField(ref, fieldPrefix+".spec", "spec is required")
	}
	return ref, nil
}

func invalidField(ref ResourceReference, fieldPath, message string) *APIError {
	return &APIError{
		Status:    http.StatusBadRequest,
		Code:      CodeInvalidExtension,
		Message:   message,
		Resource:  &ref,
		FieldPath: fieldPath,
	}
}

func ReadReplaceRequest(r *http.Request) (ReplaceRequest, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return ReplaceRequest{}, err
	}
	if len(body) > maxRequestBodyBytes {
		return ReplaceRequest{}, fmt.Errorf("request body exceeds %d bytes", maxRequestBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var req ReplaceRequest
	if err := decoder.Decode(&req); err != nil {
		return ReplaceRequest{}, err
	}
	if req.Resources == nil {
		return ReplaceRequest{}, errors.New("resources is required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ReplaceRequest{}, errors.New("request body must contain exactly one JSON object")
		}
		return ReplaceRequest{}, err
	}
	return req, nil
}

func cloneState(state State) State {
	resources, _ := cloneResources(state.Resources)
	conditions := append([]Condition(nil), state.Conditions...)
	return State{Revision: state.Revision, Resources: resources, Conditions: conditions}
}

func cloneResources(resources []Resource) ([]Resource, error) {
	if resources == nil {
		return []Resource{}, nil
	}
	body, err := json.Marshal(resources)
	if err != nil {
		return nil, fmt.Errorf("extension resources are not JSON serializable: %w", err)
	}
	var cloned []Resource
	if err := json.Unmarshal(body, &cloned); err != nil {
		return nil, fmt.Errorf("failed to clone extension resources: %w", err)
	}
	return cloned, nil
}

func sortCapabilities(capabilities []Capability) {
	for i := 1; i < len(capabilities); i++ {
		for j := i; j > 0; j-- {
			left := capabilities[j-1].APIVersion + "\x00" + capabilities[j-1].Kind
			right := capabilities[j].APIVersion + "\x00" + capabilities[j].Kind
			if left <= right {
				break
			}
			capabilities[j-1], capabilities[j] = capabilities[j], capabilities[j-1]
		}
	}
}
