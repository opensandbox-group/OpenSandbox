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

/**
 * Creates a sandbox for the pool.
 *
 * The pool continues to own warmup preparation, idle membership, renew, and cleanup.
 * Implementations should create and return a usable [Sandbox], applying the readiness
 * options supplied in [PooledSandboxCreateContext] when applicable.
 */
fun interface PooledSandboxCreator {
    fun create(context: PooledSandboxCreateContext): Sandbox
}
