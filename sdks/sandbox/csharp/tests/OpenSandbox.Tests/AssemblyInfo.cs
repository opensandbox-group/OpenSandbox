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

using Xunit;

// ClientIpTests mutates the process-wide ClientIpDetector state, and many tests
// read it via HttpClientWrapper.ApplyDefaultHeaders. Disabling cross-class test
// parallelization avoids a race on that shared state. The suite is small, so the
// serial run cost is negligible.
[assembly: CollectionBehavior(DisableTestParallelization = true)]
