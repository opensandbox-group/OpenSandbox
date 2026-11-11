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

package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// FilesystemWorkerArg selects the internal, single-request worker entrypoint.
const FilesystemWorkerArg = "--filesystem-worker"

func parseFilesystemID(raw string) (uint32, error) {
	if raw == "" {
		return 0, fmt.Errorf("identity must be a decimal integer")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("identity must be a decimal integer")
		}
	}
	id, err := strconv.ParseUint(raw, 10, 32)
	// Linux reserves (uid_t)-1 / (gid_t)-1 for 'do not change'.
	if err != nil || id == 1<<32-1 {
		return 0, fmt.Errorf("identity must be between 0 and 4294967294")
	}
	return uint32(id), nil
}

func filesystemIdentityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Never let the ordinary handler run in the privileged parent, including
		// when parsing, credential setup, or worker startup fails.
		c.Abort()
		uid, err := parseFilesystemID(c.Param("uid"))
		if err != nil {
			c.JSON(http.StatusBadRequest, model.ErrorResponse{Code: model.ErrorCodeInvalidRequest, Message: "invalid uid: " + err.Error()})
			return
		}
		gid, err := parseFilesystemID(c.Param("gid"))
		if err != nil {
			c.JSON(http.StatusBadRequest, model.ErrorResponse{Code: model.ErrorCodeInvalidRequest, Message: "invalid gid: " + err.Error()})
			return
		}
		serveFilesystemIdentity(c, uid, gid)
	}
}
