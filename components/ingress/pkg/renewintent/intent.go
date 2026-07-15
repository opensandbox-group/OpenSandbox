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

package renewintent

import "time"

type Intent struct {
	SandboxID  string `json:"sandbox_id"`
	ObservedAt string `json:"observed_at"`
	Port       int    `json:"port,omitempty"`
	RequestURI string `json:"request_uri,omitempty"`
}

func NewIntent(sandboxID string, port int, requestURI string) Intent {
	return Intent{
		SandboxID:  sandboxID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Port:       port,
		RequestURI: requestURI,
	}
}
