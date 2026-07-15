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

package com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers

/**
 * Supported programming languages for code execution.
 *
 * This object defines the languages that are officially supported by the code interpreter.
 * When adding new languages, ensure corresponding execution environments are available.
 */
object SupportedLanguage {
    const val PYTHON = "python"
    const val JAVA = "java"
    const val GO = "go"
    const val TYPESCRIPT = "typescript"
    const val BASH = "bash"
    const val JAVASCRIPT = "javascript"
}

/**
 * Represents an execution context for code interpretation.
 *
 * A CodeContext maintains the execution environment for a specific programming
 * language, including the working directory, language configuration, and
 * persistent state across multiple code executions.
 *
 * ## Context Lifecycle
 *
 * 1. **Creation**: Context is created with language and working directory
 * 2. **Execution**: Code runs within this context, building up state
 * 3. **Persistence**: Variables, imports, and functions persist between executions
 * 4. **Cleanup**: Context can be explicitly destroyed or garbage collected
 *
 * @property id Unique identifier for this execution context
 * @property language Programming language for this context (e.g., "python", "javascript")
 * @property cwd Current working directory for code execution
 */
class CodeContext private constructor(
    val id: String?,
    val language: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var language: String = SupportedLanguage.PYTHON

        private var id: String? = null

        fun id(id: String?): Builder {
            this.id = id
            return this
        }

        fun language(language: String): Builder {
            this.language = language
            return this
        }

        fun build(): CodeContext {
            return CodeContext(
                id = id,
                language = language,
            )
        }
    }
}

/**
 * Request model for executing code within a specific context.
 *
 * This model encapsulates all the information needed to execute a piece of
 * code, including the code itself and the execution context. The context
 * determines the language interpreter, working directory, and persistent state.
 *
 * ## Usage Patterns
 *
 * ### Simple Execution
 * ```kotlin
 * val request = RunCodeRequest.builder()
 *     .code("print('Hello World')")
 *     .build()
 * ```
 *
 * ### Context-Aware Execution
 * ```kotlin
 * val context = CodeContext.builder()
 *     .id("session-123")
 *     .language("python")
 *     .cwd("/workspace")
 *     .build()
 * val request = RunCodeRequest.builder()
 *     .code("import pandas as pd; df = pd.read_csv('data.csv')")
 *     .context(context)
 *     .build()
 * ```
 *
 * @property code The source code to execute
 * @property context Optional execution context. If null, a temporary context will be created
 */
class RunCodeRequest private constructor(
    val code: String,
    val context: CodeContext,
    val handlers: ExecutionHandlers?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var code: String? = null
        private var context: CodeContext = CodeContext.builder().build()
        private var handlers: ExecutionHandlers? = null

        fun code(code: String): Builder {
            require(code.isNotBlank()) { "Code cannot be blank" }
            this.code = code
            return this
        }

        fun context(context: CodeContext): Builder {
            this.context = context
            return this
        }

        fun handlers(handlers: ExecutionHandlers?): Builder {
            this.handlers = handlers
            return this
        }

        fun build(): RunCodeRequest {
            val codeValue = code ?: throw IllegalArgumentException("Code must be specified")
            return RunCodeRequest(
                code = codeValue,
                context = context,
                handlers = handlers,
            )
        }
    }
}
