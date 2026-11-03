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

//go:build !linux

package web

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func serveFilesystemIdentity(c *gin.Context, _, _ uint32) {
	c.JSON(http.StatusNotImplemented, model.ErrorResponse{
		Code: "FILESYSTEM_IDENTITY_UNAVAILABLE", Message: "filesystem execution identity requires Linux",
	})
}

func RunFilesystemWorker() error { return fmt.Errorf("filesystem execution identity requires Linux") }
