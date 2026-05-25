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

plugins {
    java
    alias(libs.plugins.spotless)
}

group = "com.alibaba.opensandbox"
version = "1.0.0"

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

repositories {
    mavenLocal()
    exclusiveContent {
        forRepository {
            mavenLocal()
        }
        filter {
            includeGroup("com.alibaba.opensandbox")
        }
    }
    mavenCentral()
}

configurations.configureEach {
    resolutionStrategy.cacheDynamicVersionsFor(0, "seconds")
    resolutionStrategy.cacheChangingModulesFor(0, "seconds")
}

dependencies {
    // OpenSandbox Kotlin SDKs
    testImplementation("com.alibaba.opensandbox:sandbox:latest.integration")
    testImplementation("com.alibaba.opensandbox:sandbox-pool-redis:latest.integration")
    testImplementation("com.alibaba.opensandbox:code-interpreter:latest.integration")

    // Test frameworks
    testImplementation("org.junit.jupiter:junit-jupiter:5.9.2")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher:1.13.4")
}

tasks.withType<Test> {
    useJUnitPlatform()
    if (project.findProperty("skipCodeInterpreterE2E") == "true") {
        exclude("**/CodeInterpreterE2ETest.class")
    }
}

tasks.register<Test>("e2eTest") {
    description = "Runs end-to-end tests."
    group = "verification"

    useJUnitPlatform {
        includeTags("e2e")
    }
}

spotless {
    java {
        googleJavaFormat("1.19.2").aosp()
        removeUnusedImports()
        trimTrailingWhitespace()
        endWithNewline()
    }
}
