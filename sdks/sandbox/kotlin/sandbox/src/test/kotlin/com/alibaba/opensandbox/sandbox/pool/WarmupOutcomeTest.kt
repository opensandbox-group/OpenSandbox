/*
 * Copyright 2026 Alibaba Group Holding Ltd.
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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxConnectionException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxRateLimitException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxTimeoutException
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class WarmupOutcomeTest {
    @Test
    fun `error categories are stable and mutually exclusive`() {
        assertCategory(WarmupErrorCategory.RATE_LIMIT, SandboxRateLimitException(), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.HTTP_4XX, apiFailure(404), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.HTTP_5XX, apiFailure(503), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.HTTP_OTHER, apiFailure(399), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.TIMEOUT, SandboxTimeoutException(), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.TIMEOUT, SandboxReadyTimeoutException(), WarmupStage.READINESS)
        assertCategory(WarmupErrorCategory.CONNECTION, SandboxConnectionException(), WarmupStage.CREATE)
        assertCategory(WarmupErrorCategory.CALLBACK, IllegalStateException(), WarmupStage.PREPARE)
        assertCategory(WarmupErrorCategory.STATE_STORE, IllegalStateException(), WarmupStage.COMMIT)
        assertCategory(WarmupErrorCategory.UNCLASSIFIED, IllegalStateException(), WarmupStage.CREATE)
    }

    private fun assertCategory(
        expected: WarmupErrorCategory,
        failure: Throwable,
        stage: WarmupStage,
    ) {
        assertEquals(expected, classifyWarmupError(stage, failure))
    }

    private fun apiFailure(statusCode: Int): SandboxApiException =
        SandboxApiException(
            message = "http failure",
            statusCode = statusCode,
            error = SandboxError(SandboxError.UNEXPECTED_RESPONSE),
        )
}
