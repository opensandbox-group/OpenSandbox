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

//go:build !linux

package file

import (
	"errors"
	"fmt"
	"os"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

var errUnsupportedPlatform = api.Permanent(errors.New("durable file sink is supported only on Linux"))

func openNoFollow(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func openNoFollowRead(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func openNoFollowExisting(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func createNoFollowExclusive(path string) (*os.File, error) {
	return nil, errUnsupportedPlatform
}
func mkdirAllNoFollow(string, os.FileMode) error {
	return errUnsupportedPlatform
}
func syncData(*os.File) error              { return errUnsupportedPlatform }
func renameNoReplace(string, string) error { return errUnsupportedPlatform }
func syncDir(string) error                 { return errUnsupportedPlatform }
func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errUnsupportedPlatform
}
func classifyPathError(operation, path string, err error) error {
	wrapped := fmt.Errorf("%s %q: %w", operation, path, err)
	if errors.Is(err, os.ErrPermission) {
		return api.Permanent(wrapped)
	}
	return wrapped
}
