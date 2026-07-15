// Copyright 2025 Alibaba Group Holding Ltd.
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

package auth

// Auth represents authentication configuration.
type Auth struct {
	Token    string
	Username string
	Password string
}

// Validate reports which auth mode is configured.
func (a *Auth) Validate() string {
	if a.Token != "" {
		return "token"
	}
	if a.Username != "" {
		return "basic"
	}
	return "none"
}
