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

package sandbox

import (
	"os"
	"testing"
)

// TestMain disables the client-go WatchListClient feature for these unit tests.
// Since k8s.io/client-go v0.35 the informer reflector prefers the WatchList
// (streaming initial events) protocol, but the generated fake clientset's watch
// reactor never emits the required initial-events bookmark, so informers would
// never complete WaitForCacheSync. Feature-gate environment variables are read
// lazily on the first Enabled() call, so setting them in TestMain is effective.
// Production code paths talk to a real API server, which supports WatchList.
func TestMain(m *testing.M) {
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
	os.Exit(m.Run())
}
