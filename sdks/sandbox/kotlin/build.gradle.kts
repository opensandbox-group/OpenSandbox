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

@file:Suppress("UnstableApiUsage")

import com.github.jengelman.gradle.plugins.shadow.tasks.ShadowJar
import org.gradle.api.GradleException
import org.gradle.api.publish.PublishingExtension
import org.gradle.api.publish.maven.MavenPublication
import org.gradle.api.publish.maven.tasks.GenerateMavenPom
import org.gradle.api.publish.tasks.GenerateModuleMetadata
import org.jetbrains.kotlin.gradle.dsl.KotlinJvmProjectExtension

fun Project.resolveVersionFromTag(expectedTagPrefix: String): String? {
    val refName = System.getenv("GITHUB_REF_NAME") ?: System.getenv("GITHUB_REF")?.removePrefix("refs/tags/")
    val fromEnv =
        refName
            ?.trim()
            ?.takeIf { it.startsWith(expectedTagPrefix) }
            ?.removePrefix(expectedTagPrefix)
            ?.trim()
            ?.takeIf { it.isNotEmpty() }
    return fromEnv
}

buildscript {
    repositories {
        mavenCentral()
        gradlePluginPortal()
    }

    dependencies {
        classpath(libs.bundles.jackson.build)
    }
}

plugins {
    alias(libs.plugins.kotlin.jvm) apply false
    alias(libs.plugins.kotlin.serialization) apply false
    alias(libs.plugins.dokka) apply false
    alias(libs.plugins.spotless)
    alias(libs.plugins.mavenPublish) apply false
    alias(libs.plugins.shadow) apply false
}

val manualProjectVersion = project.findProperty("project.version") as String
val tagVersion =
    project.resolveVersionFromTag(
        expectedTagPrefix = "java/sandbox/v",
    )

if (tagVersion != null && tagVersion != manualProjectVersion) {
    throw GradleException(
        "Ref/tag version mismatch: expected version '$manualProjectVersion' from gradle.properties, " +
            "but got '$tagVersion' from tag 'java/sandbox/v...'. Please align the tag and project.version.",
    )
}

extra["project.version"] = manualProjectVersion

allprojects {
    group = project.findProperty("project.group") as String
    version = manualProjectVersion

    repositories {
        mavenCentral()
    }
}

configure<com.diffplug.gradle.spotless.SpotlessExtension> {
    kotlin {
        target("**/*.kt")
        targetExclude("**/build/**/*.kt", "**/bin/**/*.kt", "**/generated/**/*.kt")
        ktlint()
    }
    kotlinGradle {
        target("**/*.gradle.kts")
        ktlint()
    }
}

val kotlinJvmId = libs.plugins.kotlin.jvm.get().pluginId
val kotlinSerializationId = libs.plugins.kotlin.serialization.get().pluginId
val dokkaId = libs.plugins.dokka.get().pluginId
val mavenPublishId = libs.plugins.mavenPublish.get().pluginId
val shadowId = libs.plugins.shadow.get().pluginId

val shadedSerializationProjects = setOf("sandbox-api", "sandbox", "code-interpreter")

subprojects {
    apply(plugin = mavenPublishId)

    if (name != "sandbox-bom") {
        apply(plugin = kotlinJvmId)
        apply(plugin = kotlinSerializationId)
        apply(plugin = dokkaId)

        configure<KotlinJvmProjectExtension> {
            jvmToolchain(8)
            compilerOptions {
                javaParameters.set(true)
                freeCompilerArgs.add("-Xjvm-default=all")
            }
        }
    }

    if (name in shadedSerializationProjects) {
        apply(plugin = shadowId)

        tasks.named<ShadowJar>("shadowJar").configure {
            archiveClassifier.set("shadowed")
            relocate("kotlinx.serialization", "com.alibaba.opensandbox.shaded.kotlinx.serialization")
        }
        tasks.withType<GenerateModuleMetadata>().configureEach {
            enabled = false
        }
        tasks.withType<GenerateMavenPom>().configureEach {
            doLast {
                val pomFile = destination
                val filtered =
                    pomFile
                        .readText()
                        .replace(
                            Regex(
                                "\\s*<dependency>\\s*<groupId>org\\.jetbrains\\.kotlinx</groupId>.*?</dependency>",
                                RegexOption.DOT_MATCHES_ALL,
                            ),
                            "",
                        )
                pomFile.writeText(filtered)
            }
        }

        afterEvaluate {
            extensions.configure<PublishingExtension>("publishing") {
                publications.withType<MavenPublication>().configureEach {
                    artifacts.removeIf { artifact ->
                        artifact.classifier == null && artifact.extension == "jar"
                    }
                    artifact(tasks.named("shadowJar")) {
                        classifier = null
                        extension = "jar"
                    }
                    pom.withXml {
                        val dependenciesNode =
                            asNode()
                                .children()
                                .filterIsInstance<groovy.util.Node>()
                                .firstOrNull { it.name() == "dependencies" }
                        dependenciesNode
                            ?.children()
                            ?.removeAll { child ->
                                val dependencyNode = child as? groovy.util.Node ?: return@removeAll false
                                val groupId =
                                    dependencyNode
                                        .children()
                                        .filterIsInstance<groovy.util.Node>()
                                        .firstOrNull { it.name() == "groupId" }
                                        ?.text()
                                groupId == "org.jetbrains.kotlinx"
                            }
                    }
                }
            }
        }
    }

    // Include license file in published artifacts (jars/sources jars) for compliance and clarity.
    tasks.withType<Jar>().configureEach {
        from(rootProject.file("LICENSE")) {
            into("META-INF")
        }
    }

    configure<com.vanniktech.maven.publish.MavenPublishBaseExtension> {
        coordinates(project.group.toString(), project.name, project.version.toString())
        publishToMavenCentral()
        if (!gradle.startParameter.taskNames.any { it.contains("publishToMavenLocal") }) {
            signAllPublications()
        }
        pom {
            name.set(project.name)
            description.set("Alibaba Open Sandbox SDK")
            inceptionYear.set("2025")
            url.set("https://github.com/opensandbox-group/OpenSandbox")
            licenses {
                license {
                    name.set("The Apache License, Version 2.0")
                    url.set("https://www.apache.org/licenses/LICENSE-2.0.txt")
                    distribution.set("repo")
                }
            }
            developers {
                developer {
                    id.set("alibaba")
                    name.set("Alibaba Group")
                    url.set("https://github.com/alibaba")
                    email.set("ninan.nn@alibaba-inc.com")
                }
            }
            scm {
                url.set("https://github.com/opensandbox-group/OpenSandbox")
                connection.set("scm:git:https://github.com/opensandbox-group/OpenSandbox.git")
                developerConnection.set("scm:git:ssh://git@github.com/opensandbox-group/OpenSandbox.git")
            }
            if (project.name in shadedSerializationProjects) {
                withXml {
                    val dependenciesNode =
                        asNode()
                            .children()
                            .filterIsInstance<groovy.util.Node>()
                            .firstOrNull { it.name() == "dependencies" }
                    dependenciesNode
                        ?.children()
                        ?.removeIf { child ->
                            val dependencyNode = child as? groovy.util.Node ?: return@removeIf false
                            val groupId =
                                dependencyNode
                                    .children()
                                    .filterIsInstance<groovy.util.Node>()
                                    .firstOrNull { it.name() == "groupId" }
                                    ?.text()
                            groupId == "org.jetbrains.kotlinx"
                        }
                }
            }
        }
    }
}
