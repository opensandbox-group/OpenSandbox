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

package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type basicController struct {
	ctx          *gin.Context
	sseSetupOnce sync.Once
}

func newBasicController(ctx *gin.Context) *basicController {
	return &basicController{ctx: ctx}
}

func (c *basicController) RespondError(status int, code model.ErrorCode, message ...string) {
	resp := model.ErrorResponse{
		Code:    code,
		Message: "",
	}
	if len(message) > 0 {
		resp.Message = message[0]
	}
	c.ctx.JSON(status, resp)
}

func (c *basicController) RespondSuccess(data any) {
	if data == nil {
		c.ctx.Status(http.StatusOK)
		return
	}
	c.ctx.JSON(http.StatusOK, data)
}

func (c *basicController) QueryInt64(query string, defaultValue int64) int64 {
	val, err := strconv.ParseInt(query, 10, 64)
	if err != nil {
		return defaultValue
	}
	return val
}

func (c *basicController) bindJSON(target any) error {
	decoder := json.NewDecoder(c.ctx.Request.Body)
	return decoder.Decode(target)
}
