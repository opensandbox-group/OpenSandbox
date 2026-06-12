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

package com.alibaba.opensandbox.sandbox.domain.models.sandboxes

/**
 * Credential Vault proxy startup settings.
 *
 * This model mirrors the lifecycle API shape. Credential values and bindings
 * are still managed through the sandbox-scoped egress Credential Vault API.
 */
class CredentialProxyConfig private constructor(
    val enabled: Boolean,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()

        @JvmStatic
        fun enabled(): CredentialProxyConfig = builder().enabled(true).build()
    }

    class Builder {
        private var enabled: Boolean = false

        fun enabled(enabled: Boolean): Builder {
            this.enabled = enabled
            return this
        }

        fun build(): CredentialProxyConfig = CredentialProxyConfig(enabled)
    }
}

/**
 * Write-only inline credential material for Credential Vault.
 */
class InlineCredentialSource private constructor(
    val value: String,
    val type: String,
) {
    companion object {
        const val TYPE_INLINE = "inline"

        @JvmStatic
        fun builder(): Builder = Builder()

        @JvmStatic
        fun of(value: String): InlineCredentialSource = builder().value(value).build()
    }

    class Builder {
        private var value: String? = null
        private var type: String = TYPE_INLINE

        fun value(value: String): Builder {
            require(value.isNotEmpty()) { "Credential source value cannot be empty" }
            this.value = value
            return this
        }

        fun type(type: String): Builder {
            require(type == TYPE_INLINE) { "Credential source type must be inline" }
            this.type = type
            return this
        }

        fun build(): InlineCredentialSource {
            val valueValue = value ?: throw IllegalArgumentException("Credential source value must be specified")
            return InlineCredentialSource(value = valueValue, type = type)
        }
    }
}

/**
 * Sandbox-local Credential Vault credential.
 */
class Credential private constructor(
    val name: String,
    val source: InlineCredentialSource,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var name: String? = null
        private var source: InlineCredentialSource? = null

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Credential name cannot be blank" }
            this.name = name
            return this
        }

        fun source(source: InlineCredentialSource): Builder {
            this.source = source
            return this
        }

        fun inlineSource(value: String): Builder {
            this.source = InlineCredentialSource.of(value)
            return this
        }

        fun build(): Credential {
            val nameValue = name ?: throw IllegalArgumentException("Credential name must be specified")
            val sourceValue = source ?: throw IllegalArgumentException("Credential source must be specified")
            return Credential(name = nameValue, source = sourceValue)
        }
    }
}

/**
 * Request match for a Credential Vault binding.
 */
class CredentialMatch private constructor(
    val schemes: List<Scheme>?,
    val ports: List<Int>?,
    val hosts: List<String>,
    val methods: List<String>?,
    val paths: List<String>?,
) {
    enum class Scheme {
        HTTPS,
        HTTP,
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var schemes: List<Scheme>? = null
        private var ports: List<Int>? = null
        private var hosts: List<String>? = null
        private var methods: List<String>? = null
        private var paths: List<String>? = null

        fun schemes(schemes: List<Scheme>): Builder {
            require(schemes.isNotEmpty()) { "Credential match schemes cannot be empty when provided" }
            this.schemes = schemes.toList()
            return this
        }

        fun schemes(vararg schemes: Scheme): Builder = schemes(schemes.toList())

        fun ports(ports: List<Int>): Builder {
            require(ports.isNotEmpty()) { "Credential match ports cannot be empty when provided" }
            require(ports.all { it in 1..65535 }) { "Credential match ports must be between 1 and 65535" }
            this.ports = ports.toList()
            return this
        }

        fun ports(vararg ports: Int): Builder = ports(ports.toList())

        fun hosts(hosts: List<String>): Builder {
            require(hosts.isNotEmpty()) { "Credential match hosts cannot be empty" }
            require(hosts.all { it.isNotBlank() }) { "Credential match host cannot be blank" }
            this.hosts = hosts.toList()
            return this
        }

        fun hosts(vararg hosts: String): Builder = hosts(hosts.toList())

        fun methods(methods: List<String>): Builder {
            require(methods.isNotEmpty()) { "Credential match methods cannot be empty when provided" }
            require(methods.all { it.isNotBlank() }) { "Credential match method cannot be blank" }
            this.methods = methods.toList()
            return this
        }

        fun methods(vararg methods: String): Builder = methods(methods.toList())

        fun paths(paths: List<String>): Builder {
            require(paths.isNotEmpty()) { "Credential match paths cannot be empty when provided" }
            require(paths.all { it.isNotBlank() }) { "Credential match path cannot be blank" }
            this.paths = paths.toList()
            return this
        }

        fun paths(vararg paths: String): Builder = paths(paths.toList())

        fun build(): CredentialMatch {
            val hostsValue = hosts ?: throw IllegalArgumentException("Credential match hosts must be specified")
            return CredentialMatch(
                schemes = schemes,
                ports = ports,
                hosts = hostsValue,
                methods = methods,
                paths = paths,
            )
        }
    }
}

/**
 * Custom header injection entry.
 */
class CustomHeaderEntry private constructor(
    val name: String,
    val credential: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var name: String? = null
        private var credential: String? = null

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Custom header name cannot be blank" }
            this.name = name
            return this
        }

        fun credential(credential: String): Builder {
            require(credential.isNotBlank()) { "Custom header credential cannot be blank" }
            this.credential = credential
            return this
        }

        fun build(): CustomHeaderEntry {
            val nameValue = name ?: throw IllegalArgumentException("Custom header name must be specified")
            val credentialValue = credential ?: throw IllegalArgumentException("Custom header credential must be specified")
            return CustomHeaderEntry(name = nameValue, credential = credentialValue)
        }
    }
}

/**
 * Typed Credential Vault auth rule.
 */
class CredentialAuth private constructor(
    val type: Type,
    val credential: String?,
    val name: String?,
    val headers: List<CustomHeaderEntry>?,
) {
    enum class Type {
        BEARER,
        BASIC,
        API_KEY,
        CUSTOM_HEADERS,
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()

        @JvmStatic
        fun bearer(credential: String): CredentialAuth = builder().type(Type.BEARER).credential(credential).build()

        @JvmStatic
        fun basic(credential: String): CredentialAuth = builder().type(Type.BASIC).credential(credential).build()

        @JvmStatic
        fun apiKey(
            name: String,
            credential: String,
        ): CredentialAuth = builder().type(Type.API_KEY).name(name).credential(credential).build()

        @JvmStatic
        fun customHeaders(headers: List<CustomHeaderEntry>): CredentialAuth = builder().type(Type.CUSTOM_HEADERS).headers(headers).build()
    }

    class Builder {
        private var type: Type? = null
        private var credential: String? = null
        private var name: String? = null
        private var headers: List<CustomHeaderEntry>? = null

        fun type(type: Type): Builder {
            this.type = type
            return this
        }

        fun credential(credential: String): Builder {
            require(credential.isNotBlank()) { "Credential auth credential cannot be blank" }
            this.credential = credential
            return this
        }

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Credential auth name cannot be blank" }
            this.name = name
            return this
        }

        fun headers(headers: List<CustomHeaderEntry>): Builder {
            require(headers.isNotEmpty()) { "Credential auth headers cannot be empty" }
            this.headers = headers.toList()
            return this
        }

        fun headers(vararg headers: CustomHeaderEntry): Builder = headers(headers.toList())

        fun build(): CredentialAuth {
            val typeValue = type ?: throw IllegalArgumentException("Credential auth type must be specified")
            when (typeValue) {
                Type.BEARER, Type.BASIC -> {
                    if (credential == null) {
                        throw IllegalArgumentException("Credential auth credential must be specified")
                    }
                }
                Type.API_KEY -> {
                    if (name == null || credential == null) {
                        throw IllegalArgumentException("API key auth name and credential must be specified")
                    }
                }
                Type.CUSTOM_HEADERS -> {
                    if (headers.isNullOrEmpty()) {
                        throw IllegalArgumentException("Custom headers auth headers must be specified")
                    }
                }
            }
            return CredentialAuth(type = typeValue, credential = credential, name = name, headers = headers)
        }
    }
}

/**
 * Sandbox-local Credential Vault binding.
 */
class CredentialBinding private constructor(
    val name: String,
    val match: CredentialMatch,
    val auth: CredentialAuth,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var name: String? = null
        private var match: CredentialMatch? = null
        private var auth: CredentialAuth? = null

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Credential binding name cannot be blank" }
            this.name = name
            return this
        }

        fun match(match: CredentialMatch): Builder {
            this.match = match
            return this
        }

        fun auth(auth: CredentialAuth): Builder {
            this.auth = auth
            return this
        }

        fun build(): CredentialBinding {
            val nameValue = name ?: throw IllegalArgumentException("Credential binding name must be specified")
            val matchValue = match ?: throw IllegalArgumentException("Credential binding match must be specified")
            val authValue = auth ?: throw IllegalArgumentException("Credential binding auth must be specified")
            return CredentialBinding(name = nameValue, match = matchValue, auth = authValue)
        }
    }
}

/**
 * Credential Vault create request.
 */
class CredentialVaultCreateRequest private constructor(
    val credentials: List<Credential>,
    val bindings: List<CredentialBinding>,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var credentials: List<Credential> = emptyList()
        private var bindings: List<CredentialBinding> = emptyList()

        fun credentials(credentials: List<Credential>): Builder {
            this.credentials = credentials.toList()
            return this
        }

        fun bindings(bindings: List<CredentialBinding>): Builder {
            this.bindings = bindings.toList()
            return this
        }

        fun build(): CredentialVaultCreateRequest = CredentialVaultCreateRequest(credentials = credentials, bindings = bindings)
    }
}

/**
 * Atomic credential mutation set for Credential Vault patch.
 */
class CredentialMutationSet private constructor(
    val add: List<Credential>?,
    val replace: List<Credential>?,
    val delete: List<String>?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var add: List<Credential>? = null
        private var replace: List<Credential>? = null
        private var delete: List<String>? = null

        fun add(add: List<Credential>): Builder {
            this.add = add.toList()
            return this
        }

        fun replace(replace: List<Credential>): Builder {
            this.replace = replace.toList()
            return this
        }

        fun delete(delete: List<String>): Builder {
            require(delete.all { it.isNotBlank() }) { "Credential delete name cannot be blank" }
            this.delete = delete.toList()
            return this
        }

        fun delete(vararg delete: String): Builder = delete(delete.toList())

        fun build(): CredentialMutationSet = CredentialMutationSet(add = add, replace = replace, delete = delete)
    }
}

/**
 * Atomic binding mutation set for Credential Vault patch.
 */
class CredentialBindingMutationSet private constructor(
    val add: List<CredentialBinding>?,
    val replace: List<CredentialBinding>?,
    val delete: List<String>?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var add: List<CredentialBinding>? = null
        private var replace: List<CredentialBinding>? = null
        private var delete: List<String>? = null

        fun add(add: List<CredentialBinding>): Builder {
            this.add = add.toList()
            return this
        }

        fun replace(replace: List<CredentialBinding>): Builder {
            this.replace = replace.toList()
            return this
        }

        fun delete(delete: List<String>): Builder {
            require(delete.all { it.isNotBlank() }) { "Credential binding delete name cannot be blank" }
            this.delete = delete.toList()
            return this
        }

        fun delete(vararg delete: String): Builder = delete(delete.toList())

        fun build(): CredentialBindingMutationSet = CredentialBindingMutationSet(add = add, replace = replace, delete = delete)
    }
}

/**
 * Credential Vault patch request.
 */
class CredentialVaultPatchRequest private constructor(
    val expectedRevision: Int?,
    val credentials: CredentialMutationSet?,
    val bindings: CredentialBindingMutationSet?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var expectedRevision: Int? = null
        private var credentials: CredentialMutationSet? = null
        private var bindings: CredentialBindingMutationSet? = null

        fun expectedRevision(expectedRevision: Int?): Builder {
            this.expectedRevision = expectedRevision
            return this
        }

        fun credentials(credentials: CredentialMutationSet?): Builder {
            this.credentials = credentials
            return this
        }

        fun bindings(bindings: CredentialBindingMutationSet?): Builder {
            this.bindings = bindings
            return this
        }

        fun build(): CredentialVaultPatchRequest =
            CredentialVaultPatchRequest(
                expectedRevision = expectedRevision,
                credentials = credentials,
                bindings = bindings,
            )
    }
}

/**
 * Sanitized credential metadata returned by Credential Vault.
 */
class CredentialMetadata(
    val name: String,
    val sourceType: String,
    val revision: Int,
)

/**
 * Sanitized auth metadata returned for a Credential Vault binding.
 */
class CredentialAuthMetadata(
    val type: String,
    val name: String?,
)

/**
 * Sanitized binding metadata returned by Credential Vault.
 */
class CredentialBindingMetadata(
    val name: String,
    val revision: Int,
    val match: CredentialMatch?,
    val auth: CredentialAuthMetadata?,
)

/**
 * Sanitized Credential Vault state.
 */
class CredentialVaultState(
    val revision: Int,
    val credentials: List<CredentialMetadata>,
    val bindings: List<CredentialBindingMetadata>,
)

/**
 * Sanitized Credential Vault credential list response.
 */
class CredentialListResponse(
    val revision: Int,
    val credentials: List<CredentialMetadata>,
)

/**
 * Sanitized Credential Vault binding list response.
 */
class CredentialBindingListResponse(
    val revision: Int,
    val bindings: List<CredentialBindingMetadata>,
)
