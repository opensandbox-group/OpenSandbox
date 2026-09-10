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
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
)

func handleActiveVaultSnapshot(
	w http.ResponseWriter,
	r *http.Request,
	store *credentialvault.Store,
) {
	knownTag, err := parseActiveVaultETag(r.Header.Get("If-None-Match"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	snapshot, tag, changed, err := store.ActiveSnapshotIfChanged(r.Context(), knownTag)
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}

	w.Header().Set("ETag", formatActiveVaultETag(tag))
	if !changed {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func parseActiveVaultETag(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("If-None-Match must be a quoted active-vault tag")
	}
	tag, err := strconv.Unquote(value)
	if err != nil || len(tag) == 0 || len(tag) > 128 {
		return "", fmt.Errorf("If-None-Match contains an invalid active-vault tag")
	}
	for _, char := range tag {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && !strings.ContainsRune("-._~", char) {
			return "", fmt.Errorf("If-None-Match contains an invalid active-vault tag")
		}
	}
	return tag, nil
}

func formatActiveVaultETag(tag string) string {
	return strconv.Quote(tag)
}
