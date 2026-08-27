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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import kotlinx.serialization.KSerializer
import kotlinx.serialization.descriptors.elementNames
import kotlinx.serialization.encoding.Decoder
import kotlinx.serialization.encoding.Encoder
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonDecoder
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonEncoder
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject

@JvmSynthetic
internal val jsonParser =
    Json {
        ignoreUnknownKeys = true
        isLenient = true
        encodeDefaults = true
        coerceInputValues = true
    }

abstract class AbstractUnknownPropertiesSerializer<T>(
    private val delegate: KSerializer<T>,
) : KSerializer<T> {
    override val descriptor = delegate.descriptor

    abstract fun T.withUnknownProperties(unknown: Map<String, JsonElement>): T

    abstract fun T.getUnknownProperties(): Map<String, JsonElement>

    override fun deserialize(decoder: Decoder): T {
        require(decoder is JsonDecoder)

        val jsonObject = decoder.decodeJsonElement().jsonObject

        val knownKeys = delegate.descriptor.elementNames.toSet()

        val unknownProperties = jsonObject.filterKeys { it !in knownKeys }

        val cleanJsonObject = JsonObject(jsonObject.filterKeys { it in knownKeys })
        val standardObject = decoder.json.decodeFromJsonElement(delegate, cleanJsonObject)

        return standardObject.withUnknownProperties(unknownProperties)
    }

    override fun serialize(
        encoder: Encoder,
        value: T,
    ) {
        require(encoder is JsonEncoder)

        val standardJsonElement = encoder.json.encodeToJsonElement(delegate, value)
        val standardJsonObject = standardJsonElement.jsonObject

        val unknownProperties = value.getUnknownProperties()

        val mergedMap = standardJsonObject.toMutableMap()
        mergedMap.putAll(unknownProperties)

        encoder.encodeJsonElement(JsonObject(mergedMap))
    }
}
