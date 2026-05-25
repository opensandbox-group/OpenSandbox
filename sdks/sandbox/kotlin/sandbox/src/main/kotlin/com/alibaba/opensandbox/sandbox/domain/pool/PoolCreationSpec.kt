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

package com.alibaba.opensandbox.sandbox.domain.pool

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume

/**
 * Template for creating sandboxes in the pool (replenish and direct-create).
 *
 * Pool always uses a fixed 24h timeout for created sandboxes; other parameters
 * are taken from this spec. Defaults align with [Sandbox.Builder].
 *
 * @property imageSpec Container image specification (required).
 * @property entrypoint Entrypoint command (default: tail -f /dev/null).
 * @property resource Resource limits (default: cpu=1, memory=2Gi).
 * @property env Environment variables.
 * @property metadata User-defined metadata.
 * @property extensions Optional extension parameters for server-side customized behaviors.
 * @property networkPolicy Optional outbound network policy.
 * @property platform Optional runtime platform constraint.
 * @property secureAccess Whether to enable secured access for sandbox endpoints.
 * @property volumes Optional volume mounts.
 */
class PoolCreationSpec private constructor(
    val imageSpec: SandboxImageSpec,
    val entrypoint: List<String> = DEFAULT_ENTRYPOINT,
    val resource: Map<String, String> = DEFAULT_RESOURCE,
    val env: Map<String, String> = emptyMap(),
    val metadata: Map<String, String> = emptyMap(),
    val extensions: Map<String, String> = emptyMap(),
    val networkPolicy: NetworkPolicy? = null,
    val platform: PlatformSpec? = null,
    val secureAccess: Boolean = false,
    val volumes: List<Volume>? = null,
) {
    companion object {
        /** Default entrypoint: keep container running. */
        val DEFAULT_ENTRYPOINT: List<String> = listOf("tail", "-f", "/dev/null")

        /** Default resource limits. */
        val DEFAULT_RESOURCE: Map<String, String> =
            mapOf(
                "cpu" to "1",
                "memory" to "2Gi",
            )

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var imageSpec: SandboxImageSpec? = null
        private var entrypoint: List<String> = DEFAULT_ENTRYPOINT
        private var resource: Map<String, String> = DEFAULT_RESOURCE
        private var env: Map<String, String> = emptyMap()
        private var metadata: Map<String, String> = emptyMap()
        private var extensions: Map<String, String> = emptyMap()
        private var networkPolicy: NetworkPolicy? = null
        private var platform: PlatformSpec? = null
        private var secureAccess: Boolean = false
        private var volumes: List<Volume>? = null

        fun imageSpec(imageSpec: SandboxImageSpec): Builder {
            this.imageSpec = imageSpec
            return this
        }

        fun image(image: String): Builder {
            this.imageSpec = SandboxImageSpec.builder().image(image).build()
            return this
        }

        fun entrypoint(entrypoint: List<String>): Builder {
            this.entrypoint = entrypoint
            return this
        }

        fun entrypoint(vararg entrypoint: String): Builder {
            this.entrypoint = entrypoint.toList()
            return this
        }

        fun resource(resource: Map<String, String>): Builder {
            this.resource = resource
            return this
        }

        fun resource(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = DEFAULT_RESOURCE.toMutableMap()
            map.configure()
            this.resource = map
            return this
        }

        fun env(env: Map<String, String>): Builder {
            this.env = env
            return this
        }

        fun env(
            key: String,
            value: String,
        ): Builder {
            require(key.isNotBlank()) { "Environment variable key cannot be blank" }
            this.env = this.env + (key to value)
            return this
        }

        fun env(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = this.env.toMutableMap()
            map.configure()
            this.env = map
            return this
        }

        fun metadata(metadata: Map<String, String>): Builder {
            this.metadata = metadata
            return this
        }

        fun metadata(
            key: String,
            value: String,
        ): Builder {
            require(key.isNotBlank()) { "Metadata key cannot be blank" }
            this.metadata = this.metadata + (key to value)
            return this
        }

        fun metadata(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = this.metadata.toMutableMap()
            map.configure()
            this.metadata = map
            return this
        }

        fun extension(
            key: String,
            value: String,
        ): Builder {
            require(key.isNotBlank()) { "Extension key cannot be blank" }
            this.extensions = this.extensions + (key to value)
            return this
        }

        fun extensions(extensions: Map<String, String>): Builder {
            this.extensions = this.extensions + extensions
            return this
        }

        fun extensions(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = this.extensions.toMutableMap()
            map.configure()
            this.extensions = map
            return this
        }

        fun networkPolicy(networkPolicy: NetworkPolicy?): Builder {
            this.networkPolicy = networkPolicy
            return this
        }

        fun networkPolicy(configure: NetworkPolicy.Builder.() -> Unit): Builder {
            val builder = NetworkPolicy.builder()
            builder.configure()
            this.networkPolicy = builder.build()
            return this
        }

        fun platform(platform: PlatformSpec?): Builder {
            this.platform = platform
            return this
        }

        fun platform(configure: PlatformSpec.Builder.() -> Unit): Builder {
            val builder = PlatformSpec.builder()
            builder.configure()
            this.platform = builder.build()
            return this
        }

        @JvmOverloads
        fun secureAccess(enabled: Boolean = true): Builder {
            this.secureAccess = enabled
            return this
        }

        fun volumes(volumes: List<Volume>?): Builder {
            this.volumes = volumes
            return this
        }

        fun volume(volume: Volume): Builder {
            this.volumes = (this.volumes ?: emptyList()) + volume
            return this
        }

        fun volume(configure: Volume.Builder.() -> Unit): Builder {
            val builder = Volume.builder()
            builder.configure()
            return volume(builder.build())
        }

        fun build(): PoolCreationSpec {
            val spec = imageSpec ?: throw IllegalArgumentException("PoolCreationSpec imageSpec (or image) must be specified")
            return PoolCreationSpec(
                imageSpec = spec,
                entrypoint = entrypoint,
                resource = resource,
                env = env,
                metadata = metadata,
                extensions = extensions,
                networkPolicy = networkPolicy,
                platform = platform,
                secureAccess = secureAccess,
                volumes = volumes,
            )
        }
    }
}
