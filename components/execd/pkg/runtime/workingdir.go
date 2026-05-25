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

package runtime

import (
	"fmt"
	"os"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

func ValidateWorkingDir(cwd string) error {
	if cwd == "" {
		return nil
	}
	resolvedCwd, err := pathutil.ExpandPath(cwd)
	if err != nil {
		return fmt.Errorf("cannot resolve working directory %q: %w", cwd, err)
	}
	fi, err := os.Stat(resolvedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("working directory does not exist: %s", cwd)
		}
		return fmt.Errorf("cannot access working directory %q: %w", cwd, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("working directory path is not a directory: %s", cwd)
	}
	return nil
}
