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

package policy

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Operator-managed list of domain patterns whose successful DNS resolutions
// should not produce an "egress outbound" log line. Missing file is ignored.
// One-shot load at startup; not hot-reloaded.
const logSkipFilePath = "/var/egress/rules/log_skip.always"

// LoadLogSkipFile reads optional /var/egress/rules/log_skip.always.
// Each non-empty/non-comment line is a domain pattern: "foo.com" (exact) or
// "*.foo.com" (subdomain wildcard). IP/CIDR entries are rejected — only
// host/domain entries are meaningful for matching against DNS query names.
func LoadLogSkipFile() ([]string, error) {
	return loadLogSkipFile(logSkipFilePath)
}

func loadLogSkipFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseLogSkipLines(data, path)
}

func parseLogSkipLines(data []byte, pathForErr string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := ParseValidatedEgressRule(ActionAllow, line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", pathForErr, lineNum, err)
		}
		if rule.targetKind != targetDomain {
			return nil, fmt.Errorf("%s line %d: %q is not a domain pattern; only host/domain entries are supported", pathForErr, lineNum, line)
		}
		out = append(out, rule.Target)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", pathForErr, err)
	}
	return out, nil
}
