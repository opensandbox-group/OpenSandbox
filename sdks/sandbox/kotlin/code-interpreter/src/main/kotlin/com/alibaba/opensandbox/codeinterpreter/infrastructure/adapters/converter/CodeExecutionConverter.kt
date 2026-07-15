/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.codeinterpreter.infrastructure.adapters.converter

import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.CodeContext
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.RunCodeRequest
import com.alibaba.opensandbox.sandbox.api.models.execd.CodeContext as ApiCodeContext
import com.alibaba.opensandbox.sandbox.api.models.execd.RunCodeRequest as ApiRunCodeRequest

object CodeExecutionConverter {
    fun RunCodeRequest.toApiRunCodeRequest(): ApiRunCodeRequest {
        return ApiRunCodeRequest(
            code = this.code,
            context = this.context?.toApiCodeContext(),
        )
    }

    fun CodeContext.toApiCodeContext(): ApiCodeContext {
        return ApiCodeContext(
            id = this.id,
            language = this.language,
        )
    }

    fun ApiCodeContext.toCodeContext(): CodeContext {
        return CodeContext.builder()
            .id(this.id)
            .language(this.language)
            .build()
    }
}
