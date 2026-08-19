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

package sink

import (
	"bytes"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

func SameResourceIdentity(left, right api.Resource) bool {
	return left == right
}

func EncodeBatch(batch api.Batch) []byte {
	var out bytes.Buffer
	for _, item := range batch.Items {
		out.WriteString(item.Record.Timestamp.UTC().Format(time.RFC3339Nano))
		out.WriteByte(' ')
		out.WriteString(item.Record.Attributes["stream"])
		out.WriteByte(' ')
		out.Write(item.Record.Body)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
