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

import org.openapitools.generator.gradle.plugin.tasks.GenerateTask

plugins {
    alias(libs.plugins.openapi.generator)
}

repositories {
    mavenCentral()
}

dependencies {
    implementation(libs.okhttp)
    implementation(libs.bundles.serialization)
}

fun GenerateTask.configureCommonOptions() {
    outputs.doNotCacheIf("OpenAPI generation must reflect the current spec files") { true }

    generatorName.set("kotlin")
    library.set("jvm-okhttp4")

    typeMappings.set(
        mapOf(
            "object" to "kotlinx.serialization.json.JsonElement",
            "Object" to "kotlinx.serialization.json.JsonElement",
            "java.lang.Object" to "kotlinx.serialization.json.JsonElement",
            "Any" to "kotlinx.serialization.json.JsonElement",
            "kotlin.Any" to "kotlinx.serialization.json.JsonElement",
            "binary" to "java.io.InputStream",
            "file" to "java.io.InputStream",
        ),
    )

    importMappings.set(
        mapOf(
            "JsonElement" to "kotlinx.serialization.json.JsonElement",
        ),
    )

    configOptions.set(
        mapOf(
            "jvm8" to "true",
            "coroutine" to "false",
            "dateLibrary" to "java8",
            "serializationLibrary" to "kotlinx_serialization",
            "documentationProvider" to "kdoc",
            "useKtor" to "false",
            "omitGradleWrapper" to "true",
        ),
    )

    globalProperties.set(
        mapOf(
            "apiTests" to "false",
            "modelTests" to "false",
        ),
    )
}

val generateSandboxLifecycleApi =
    tasks.register<GenerateTask>("generateSandboxLifecycleApi") {
        configureCommonOptions()

        inputSpec.set(
            rootProject.projectDir.parentFile.parentFile.parentFile
                .resolve("specs/sandbox-lifecycle.yml").absolutePath,
        )
        outputDir.set(layout.buildDirectory.dir("generated/api/lifecycle").get().asFile.absolutePath)
        packageName.set("com.alibaba.opensandbox.sandbox.api")
        apiPackage.set("com.alibaba.opensandbox.sandbox.api")
        modelPackage.set("com.alibaba.opensandbox.sandbox.api.models")
    }

val generateExecdApi =
    tasks.register<GenerateTask>("generateExecdApi") {
        configureCommonOptions()

        inputSpec.set(rootProject.projectDir.parentFile.parentFile.parentFile.resolve("specs/execd-api.yaml").absolutePath)
        outputDir.set(layout.buildDirectory.dir("generated/api/execd").get().asFile.absolutePath)
        packageName.set("com.alibaba.opensandbox.sandbox.api.execd")
        apiPackage.set("com.alibaba.opensandbox.sandbox.api.execd")
        modelPackage.set("com.alibaba.opensandbox.sandbox.api.models.execd")
    }

val generateEgressApi =
    tasks.register<GenerateTask>("generateEgressApi") {
        configureCommonOptions()

        inputSpec.set(rootProject.projectDir.parentFile.parentFile.parentFile.resolve("specs/egress-api.yaml").absolutePath)
        outputDir.set(layout.buildDirectory.dir("generated/api/egress").get().asFile.absolutePath)
        packageName.set("com.alibaba.opensandbox.sandbox.api.egress")
        apiPackage.set("com.alibaba.opensandbox.sandbox.api.egress")
        modelPackage.set("com.alibaba.opensandbox.sandbox.api.models.egress")
    }

val generateDiagnosticApi =
    tasks.register<GenerateTask>("generateDiagnosticApi") {
        configureCommonOptions()

        inputSpec.set(rootProject.projectDir.parentFile.parentFile.parentFile.resolve("specs/diagnostic-api.yml").absolutePath)
        outputDir.set(layout.buildDirectory.dir("generated/api/diagnostic").get().asFile.absolutePath)
        packageName.set("com.alibaba.opensandbox.sandbox.api.diagnostic")
        apiPackage.set("com.alibaba.opensandbox.sandbox.api.diagnostic")
        modelPackage.set("com.alibaba.opensandbox.sandbox.api.models.diagnostic")
    }

val lifecycleSrc = generateSandboxLifecycleApi.map { file(it.outputDir).resolve("src/main/kotlin") }
val execdSrc = generateExecdApi.map { file(it.outputDir).resolve("src/main/kotlin") }
val egressSrc = generateEgressApi.map { file(it.outputDir).resolve("src/main/kotlin") }
val diagnosticSrc = generateDiagnosticApi.map { file(it.outputDir).resolve("src/main/kotlin") }
sourceSets {
    main {
        java.srcDirs(lifecycleSrc, execdSrc, egressSrc, diagnosticSrc)
    }
}
