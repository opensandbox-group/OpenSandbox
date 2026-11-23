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
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type httpRange struct {
	start, length int64
}

// errRangeUnsatisfiable reports a well-formed header that selects no byte of
// the content. Callers answer it with 416 and the unsatisfied-range
// Content-Range that RFC 7233 section 4.4 asks for.
var errRangeUnsatisfiable = errors.New("invalid range: no byte selected")

func ParseRange(s string, size int64) ([]httpRange, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return nil, errors.New("invalid range")
	}

	ranges := strings.Split(s[6:], ",")
	result := make([]httpRange, 0, len(ranges))
	unsatisfiable := false

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
			if n == 0 {
				// RFC 7233 section 2.1: a suffix-length of zero selects no
				// bytes. Serving it used to produce a 206 whose Content-Range
				// ran backwards (bytes 10-9/10).
				unsatisfiable = true
				continue
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
	// Any satisfiable range in the header still wins, so only a header whose
	// every spec was dropped is reported. An empty representation counts too:
	// no range can select a byte of it, and letting it through produced
	// Content-Range: bytes 0--1/0 from the whole-file fallthrough.
	if unsatisfiable && len(result) == 0 {
		return nil, errRangeUnsatisfiable
	}
	return result, nil
}

// respondRangeError writes the reply for a Range header the download handlers
// cannot serve. A malformed header keeps the message callers have always seen;
// a well-formed header that selects no byte also reports the real length in
// Content-Range, as RFC 7233 section 4.4 asks.
func (c *basicController) respondRangeError(rangeHeader string, size int64, err error) {
	message := fmt.Sprintf("invalid Range header: %s", rangeHeader)
	if errors.Is(err, errRangeUnsatisfiable) {
		c.ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", size))
		message = fmt.Sprintf("requested range not satisfiable: %s", rangeHeader)
	}
	c.RespondError(http.StatusRequestedRangeNotSatisfiable, model.ErrorCodeUnknown, message)
}
