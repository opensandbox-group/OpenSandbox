// Copyright 2026 The OpenSandbox Authors
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

package controller

import (
	"errors"
	"strconv"
	"strings"
)

type httpRange struct {
	start, length int64
}

func ParseRange(s string, size int64) ([]httpRange, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return nil, errors.New("invalid range")
	}

	ranges := strings.Split(s[6:], ",")
	result := make([]httpRange, 0, len(ranges))

	for _, ra := range ranges {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}
		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])
		var r httpRange

		if start == "" {
			// suffix-length
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("invalid range")
			}
			if n > size {
				n = size
			}
			r.start = size - n
			r.length = size - r.start
		} else {
			// start-end
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid range")
			}
			if end == "" {
				// start-
				r.start = i
				r.length = size - i
			} else {
				// start-end
				j, err := strconv.ParseInt(end, 10, 64)
				if err != nil || j < i {
					return nil, errors.New("invalid range")
				}
				r.start = i
				// Clamp end before computing the length so a huge end cannot
				// overflow it (as net/http does).
				r.length = min(j, size-1) - i + 1
			}
		}
		if r.start >= size {
			continue
		}
		if r.start+r.length > size {
			r.length = size - r.start
		}
		result = append(result, r)
	}
	return result, nil
}
