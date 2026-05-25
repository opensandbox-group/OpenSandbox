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
    `java-platform`
}

dependencies {
    constraints {
        api(project(":sandbox"))
        api(project(":sandbox-api"))
        api(project(":sandbox-pool-redis"))

        api(libs.kotlin.stdlib)
        api(libs.okhttp)
        api(libs.okhttp.logging)
        api(libs.kotlinx.serialization.json)
        api(libs.slf4j.api)
    }
}
