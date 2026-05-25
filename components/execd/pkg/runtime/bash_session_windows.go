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

//go:build windows
// +build windows

package runtime

import (
	"context"
	"errors"
)

var errBashSessionNotSupported = errors.New("bash session is not supported on windows")

// CreateBashSession is not supported on Windows.
func (c *Controller) CreateBashSession(_ *CreateContextRequest) (string, error) { //nolint:revive
	return "", errBashSessionNotSupported
}

// RunInBashSession is not supported on Windows.
func (c *Controller) RunInBashSession(_ context.Context, _ *ExecuteCodeRequest) error { //nolint:revive
	return errBashSessionNotSupported
}

// DeleteBashSession is not supported on Windows.
func (c *Controller) DeleteBashSession(_ string) error { //nolint:revive
	return errBashSessionNotSupported
}
