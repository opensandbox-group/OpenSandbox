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

dependencies {
    implementation(project(":sandbox-api"))
    api(libs.kotlin.stdlib)
    api(libs.slf4j.api)

    implementation(libs.okhttp)
    implementation(libs.okhttp.logging)
    implementation(libs.bundles.serialization)

    testImplementation(libs.bundles.testing)
    testRuntimeOnly(libs.junit.platform.launcher)
}

// Configure test tasks to use JDK 17
tasks.withType<Test> {
    javaLauncher.set(
        javaToolchains.launcherFor {
            languageVersion.set(JavaLanguageVersion.of(17))
        },
    )
    useJUnitPlatform()
}

// Configure test compilation to use JDK 17
tasks.withType<org.jetbrains.kotlin.gradle.tasks.KotlinCompile> {
    if (name.contains("test", ignoreCase = true)) {
        compilerOptions {
            jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
        }
    }
    compilerOptions {
        javaParameters.set(true)
    }
}

tasks.withType<JavaCompile> {
    if (name.contains("test", ignoreCase = true)) {
        javaCompiler.set(
            javaToolchains.compilerFor {
                languageVersion.set(JavaLanguageVersion.of(17))
            },
        )
    }
}
