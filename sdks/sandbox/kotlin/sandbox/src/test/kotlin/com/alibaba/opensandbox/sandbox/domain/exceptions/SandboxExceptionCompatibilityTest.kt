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

package com.alibaba.opensandbox.sandbox.domain.exceptions

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class SandboxExceptionCompatibilityTest {
    @Test
    fun `base exception should keep legacy constructor signature`() {
        val ex = SandboxException("boom", null, SandboxError("INTERNAL_UNKNOWN_ERROR"))

        assertEquals("boom", ex.message)
        assertNull(ex.requestId)
    }

    @Test
    fun `api exception should keep legacy constructor signature`() {
        val ex = SandboxApiException("boom", null, 500, SandboxError("UNEXPECTED_RESPONSE"))

        assertEquals("boom", ex.message)
        assertEquals(500, ex.statusCode)
        assertNull(ex.requestId)
    }
}
