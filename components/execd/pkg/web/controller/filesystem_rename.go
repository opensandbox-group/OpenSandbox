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

package controller

import (
	"errors"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func validateRenameFiles(request []model.RenameFileItem) error {
	for _, item := range request {
		if item.Src == "" || item.Dest == "" {
			return errors.New("src and dest must not be empty")
		}
	}
	return nil
}

func validateExpandedRenameFiles(request []model.RenameFileItem) error {
	if err := validateRenameFiles(request); err != nil {
		return err
	}
	for _, item := range request {
		for _, path := range []string{item.Src, item.Dest} {
			expanded, err := pathutil.ExpandPath(path)
			// Leave expansion errors to RenameFile's existing error handling.
			if err == nil && expanded == "" {
				return errors.New("src and dest must not expand to empty paths")
			}
		}
	}
	return nil
}
