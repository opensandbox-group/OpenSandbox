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

package controller

import (
	"fmt"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// validateRenameItems checks every entry of a batch rename request before any
// filesystem work starts. An empty src or dest would otherwise be expanded to
// the process working directory (or the session view root) by
// pathutil.ExpandAbsPath and renamed instead of the intended file, so the
// whole batch must carry non-empty paths.
func validateRenameItems(items []model.RenameFileItem) error {
	for i, item := range items {
		if item.Src == "" {
			return fmt.Errorf("rename entry %d must have a non-empty src", i)
		}
		if item.Dest == "" {
			return fmt.Errorf("rename entry %d must have a non-empty dest", i)
		}
	}
	return nil
}
