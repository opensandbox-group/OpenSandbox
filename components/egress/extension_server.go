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

package main

import (
	"net/http"
	"os"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/extensions"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

func newExtensionStoreFromEnv() *extensions.Store {
	if constants.IsTruthy(os.Getenv(constants.EnvExtensionsPOC)) {
		log.Warnf("egress extensions POC is enabled: resources are stored but not enforced")
		return extensions.NewStore(extensions.POCApplier{})
	}
	return extensions.NewStore(nil)
}

func (s *policyServer) handleExtensionCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.extensions().Capabilities())
}

func (s *policyServer) handleExtensions(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.extensions().Current())
	case http.MethodPut:
		s.handleReplaceExtensions(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *policyServer) handleReplaceExtensions(w http.ResponseWriter, r *http.Request) {
	req, err := extensions.ReadReplaceRequest(r)
	if err != nil {
		writeExtensionError(w, &extensions.APIError{
			Status:  http.StatusBadRequest,
			Code:    extensions.CodeInvalidExtension,
			Message: err.Error(),
		})
		return
	}
	state, apiErr := s.extensions().Replace(r.Context(), req)
	if apiErr != nil {
		writeExtensionError(w, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *policyServer) extensions() *extensions.Store {
	if s.extensionStore == nil {
		s.extensionStore = extensions.NewStore(nil)
	}
	return s.extensionStore
}

func writeExtensionError(w http.ResponseWriter, apiErr *extensions.APIError) {
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, apiErr)
}
