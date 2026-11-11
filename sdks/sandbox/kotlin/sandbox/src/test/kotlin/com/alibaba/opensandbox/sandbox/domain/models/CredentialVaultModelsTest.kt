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

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuth
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.ExchangeCredentialSource
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.InlineCredentialSource
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.toJsonObject
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class CredentialVaultModelsTest {
    @Test
    fun `exchange source serializes the flat wire shape for uds and https endpoints`() {
        val uds = ExchangeCredentialSource.of("unix:///run/spi/spi.sock").toJsonObject()
        assertEquals("{\"type\":\"exchange\",\"endpoint\":\"unix:///run/spi/spi.sock\"}", uds.toString())

        val https = ExchangeCredentialSource.of("https://resolver.internal:8443/prefix").toJsonObject()
        assertEquals("{\"type\":\"exchange\",\"endpoint\":\"https://resolver.internal:8443/prefix\"}", https.toString())
    }

    @Test
    fun `inline source serialization bytes stay unchanged`() {
        val inline = InlineCredentialSource.of("dummy-inline-value").toJsonObject()
        assertEquals("{\"type\":\"inline\",\"value\":\"dummy-inline-value\"}", inline.toString())
    }

    @Test
    fun `typeless auth serializes credential only without a type key`() {
        val auth = CredentialAuth.exchange("spi-signer").toJsonObject()
        assertEquals("{\"credential\":\"spi-signer\"}", auth.toString())
        assertFalse(auth.toString().contains("\"type\""))
    }

    @Test
    fun `typeless auth builder requires a credential reference`() {
        assertThrows(IllegalArgumentException::class.java) {
            CredentialAuth.builder().build()
        }
    }

    @Test
    fun `typeless auth builder rejects static-only fields`() {
        assertThrows(IllegalArgumentException::class.java) {
            CredentialAuth.builder().credential("spi-signer").name("X-Api-Key").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            CredentialAuth.builder().credential("spi-signer").substitutions(emptyList()).build()
        }
    }

    @Test
    fun `static auth factories keep their validation`() {
        assertEquals(CredentialAuth.Type.BEARER, CredentialAuth.bearer("tok").type)
        assertThrows(IllegalArgumentException::class.java) {
            CredentialAuth.builder().type(CredentialAuth.Type.BEARER).build()
        }
    }

    @Test
    fun `credential builder accepts inline and exchange sources`() {
        val inline =
            Credential.builder()
                .name("api-token")
                .inlineSource("dummy-inline-value")
                .build()
        assertEquals("inline", inline.source.type)

        val exchange =
            Credential.builder()
                .name("spi-signer")
                .exchangeSource("unix:///run/spi/spi.sock")
                .build()
        assertEquals("exchange", exchange.source.type)
    }

    @Test
    fun `exchange source builder validates the endpoint`() {
        assertThrows(IllegalArgumentException::class.java) {
            ExchangeCredentialSource.builder().endpoint(" ").build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ExchangeCredentialSource.builder().build()
        }
        assertThrows(IllegalArgumentException::class.java) {
            ExchangeCredentialSource.builder().endpoint("unix:///run/spi/spi.sock").type("inline").build()
        }
    }

    @Test
    fun `toString keeps inline material hidden and the exchange endpoint visible`() {
        val inline = InlineCredentialSource.of("dummy-inline-value")
        assertFalse(inline.toString().contains("dummy-inline-value"))
        assertTrue(inline.toString().contains("inline"))

        val exchange = ExchangeCredentialSource.of("unix:///run/spi/spi.sock")
        assertTrue(exchange.toString().contains("unix:///run/spi/spi.sock"))
    }

    @Test
    fun `typeless auth model exposes the nullable type`() {
        val auth = CredentialAuth.exchange("spi-signer")
        assertNull(auth.type)
        assertEquals("spi-signer", auth.credential)
        assertNull(auth.name)
        assertNull(auth.headers)
        assertNull(auth.substitutions)
    }
}
