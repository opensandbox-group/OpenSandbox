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

package containerlogs

import (
	"io/fs"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

type unsupportedFileInfo struct{}

func (unsupportedFileInfo) Name() string       { return "unsupported.log" }
func (unsupportedFileInfo) Size() int64        { return 0 }
func (unsupportedFileInfo) Mode() fs.FileMode  { return 0 }
func (unsupportedFileInfo) ModTime() time.Time { return time.Time{} }
func (unsupportedFileInfo) IsDir() bool        { return false }
func (unsupportedFileInfo) Sys() any           { return struct{}{} }

func TestSourceFileIdentityClassifiesUnsupportedStatType(t *testing.T) {
	_, _, err := sourceFileIdentity(unsupportedFileInfo{})
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v, want permanent error", err, api.IsRetryableError(err))
	}
}
