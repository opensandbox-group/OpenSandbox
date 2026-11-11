/*
 * Copyright 2026 The OpenSandbox Authors
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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CreateTemplateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateFormat
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateReadiness
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class TemplateModelsTest {
    @Test
    fun `createTemplateRequest should require an image`() {
        val exception =
            assertThrows(IllegalArgumentException::class.java) {
                CreateTemplateRequest.builder().publish("s3://bucket/publish").build()
            }
        assertEquals("Template image must be specified", exception.message)
    }

    @Test
    fun `createTemplateRequest should require a publish target`() {
        val exception =
            assertThrows(IllegalArgumentException::class.java) {
                CreateTemplateRequest.builder().image("ubuntu:22.04").build()
            }
        assertEquals("Template publish target must be specified", exception.message)
    }

    @Test
    fun `createTemplateRequest should reject a blank image`() {
        assertThrows(IllegalArgumentException::class.java) {
            CreateTemplateRequest
                .builder()
                .image("   ")
                .publish("s3://bucket/publish")
                .build()
        }
    }

    @Test
    fun `createTemplateRequest should reject a blank publish target`() {
        assertThrows(IllegalArgumentException::class.java) {
            CreateTemplateRequest
                .builder()
                .image("ubuntu:22.04")
                .publish(" ")
                .build()
        }
    }

    @Test
    fun `createTemplateRequest should keep optional fields unset when not configured`() {
        val request =
            CreateTemplateRequest
                .builder()
                .image("ubuntu:22.04")
                .publish("s3://bucket/publish")
                .build()

        assertNull(request.resourceLimits)
        assertNull(request.entrypoint)
        assertNull(request.env)
        assertNull(request.metadata)
        assertNull(request.readiness)
        assertNull(request.format)
    }

    @Test
    fun `createTemplateRequest should keep configured fields`() {
        val request =
            CreateTemplateRequest
                .builder()
                .image("ubuntu:22.04")
                .publish("s3://bucket/publish")
                .resourceLimits {
                    put("cpu", "2")
                    put("memory", "4Gi")
                }
                .entrypoint("tail", "-f", "/dev/null")
                .env {
                    put("LOG_LEVEL", "info")
                }
                .metadata(mapOf("team" to "backend"))
                .readiness {
                    probe("tcp://127.0.0.1:44772")
                    warmupSeconds(30)
                }
                .format(TemplateFormat.OVERLAYBD)
                .build()

        assertEquals(mapOf("cpu" to "2", "memory" to "4Gi"), request.resourceLimits)
        assertEquals(listOf("tail", "-f", "/dev/null"), request.entrypoint)
        assertEquals(mapOf("LOG_LEVEL" to "info"), request.env)
        assertEquals(mapOf("team" to "backend"), request.metadata)
        assertEquals("tcp://127.0.0.1:44772", request.readiness?.probe)
        assertEquals(30, request.readiness?.warmupSeconds)
        assertEquals(TemplateFormat.OVERLAYBD, request.format)
    }

    @Test
    fun `templateReadiness should reject a blank probe`() {
        assertThrows(IllegalArgumentException::class.java) {
            TemplateReadiness.builder().probe(" ").build()
        }
    }

    @Test
    fun `templateReadiness should reject negative warmup seconds`() {
        assertThrows(IllegalArgumentException::class.java) {
            TemplateReadiness.builder().warmupSeconds(-1).build()
        }
    }

    @Test
    fun `templateFilter should reject a non positive page size`() {
        assertThrows(IllegalArgumentException::class.java) {
            TemplateFilter.builder().pageSize(0).build()
        }
    }

    @Test
    fun `templateFilter should reject a zero page`() {
        val exception =
            assertThrows(IllegalArgumentException::class.java) {
                TemplateFilter.builder().page(0).build()
            }
        assertEquals("Page must be at least 1 (1-indexed)", exception.message)
    }
}
