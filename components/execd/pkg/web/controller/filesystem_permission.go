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
	"fmt"
	"os"
	"strconv"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// permissionFileMode converts octal digits in a decimal integer (644, 4755)
// into Go's permission flags, which store the special bits separately.
func permissionFileMode(digits int) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(strconv.Itoa(digits), 8, 32)
	if err != nil || parsed > 0o7777 {
		return 0, fmt.Errorf("%d is not octal digits between 0 and 7777, such as 644 or 4755", digits)
	}

	mode := os.FileMode(parsed & 0o777)
	if parsed&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if parsed&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if parsed&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode, nil
}

// validatePermissionModes checks the entire request before any target changes,
// regardless of the iteration order of the request map. Sibling requests keep
// honoring a nonzero value retained when a duplicate mode ends in null.
func validatePermissionModes(request map[string]model.Permission) error {
	for path, perm := range request {
		if perm.ModeSet || perm.Mode != 0 {
			if _, err := permissionFileMode(perm.Mode); err != nil {
				return fmt.Errorf("invalid mode for %s: %w", path, err)
			}
		}
	}
	return nil
}

// applyFilePermission validates before chown, then applies ownership before
// mode: chown can clear setuid/setgid. Omitted/null mode skips chmod.
func applyFilePermission(path string, perm model.Permission) error {
	var mode os.FileMode
	if perm.ModeSet {
		var err error
		mode, err = permissionFileMode(perm.Mode)
		if err != nil {
			return err
		}
	}
	if err := SetFileOwnership(path, perm.Owner, perm.Group); err != nil {
		return err
	}
	if !perm.ModeSet {
		return nil
	}
	return os.Chmod(path, mode)
}
