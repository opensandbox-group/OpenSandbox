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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotStatus
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.lang.reflect.Constructor
import java.lang.reflect.Method
import java.lang.reflect.Modifier
import java.time.OffsetDateTime
import kotlin.jvm.internal.DefaultConstructorMarker

/**
 * Upgrade-compatibility regression test for the snapshot-mode API surface.
 *
 * Callers and `Sandboxes` implementations compiled against SDKs that predate
 * snapshot mode bind to exact JVM descriptors: public methods, the compiler
 * generated `$default` bridges for Kotlin default arguments, the abstract
 * two-argument interface method, and the `SnapshotInfo` constructors (public
 * and synthetic). Removing any of them fails those binaries at runtime with
 * `NoSuchMethodError`/`AbstractMethodError` instead of at compile time, so
 * this test pins each descriptor and fails CI if one disappears again.
 *
 * Method-shape reference (all descriptors in the pre-mode SDK):
 * - `Sandbox.createSnapshot(String)` + `createSnapshot$default(Sandbox, String, int, Object)`
 * - `SandboxManager.createSnapshot(String, String)` + `createSnapshot$default(SandboxManager, String, String, int, Object)`
 * - `Sandboxes.createSnapshot(String, String)` (abstract) + `createSnapshot(String, String, String)` (default method)
 * - `SnapshotInfo(String, String, String, SnapshotStatus, OffsetDateTime)`
 *   + synthetic `(..., int, DefaultConstructorMarker)`; the mode-aware
 *   six-argument constructor is additive.
 */
class SnapshotModeUpgradeCompatibilityTest {
    @Test
    fun `Sandbox keeps the pre-mode createSnapshot descriptors`() {
        val methods = Sandbox::class.java.declaredMethods

        assertNotNull(
            methods.exact("createSnapshot", String::class.java),
            "createSnapshot(String) is what pre-mode Java callers bind to",
        )
        assertNotNull(
            methods.firstOrNull { it.isDefaultBridge(Sandbox::class.java, String::class.java) },
            "createSnapshot\$default(Sandbox, String, int, Object) is what pre-mode Kotlin callers " +
                "that omit name bind to",
        )
    }

    @Test
    fun `SandboxManager keeps the pre-mode createSnapshot descriptors`() {
        val methods = SandboxManager::class.java.declaredMethods

        assertNotNull(
            methods.exact("createSnapshot", String::class.java, String::class.java),
            "createSnapshot(String, String) is what pre-mode Java callers bind to",
        )
        assertNotNull(
            methods.firstOrNull {
                it.isDefaultBridge(SandboxManager::class.java, String::class.java, String::class.java)
            },
            "createSnapshot\$default(SandboxManager, String, String, int, Object) is what pre-mode " +
                "Kotlin callers that omit name bind to",
        )
    }

    @Test
    fun `Sandboxes interface keeps the two-argument contract and the delegating default method`() {
        val twoArg = Sandboxes::class.java.getMethod("createSnapshot", String::class.java, String::class.java)
        assertTrue(
            Modifier.isAbstract(twoArg.modifiers),
            "existing Sandboxes implementations implement exactly the two-argument createSnapshot; " +
                "making it concrete or removing it breaks them",
        )

        val threeArg =
            Sandboxes::class.java.getMethod("createSnapshot", String::class.java, String::class.java, String::class.java)
        assertTrue(
            !Modifier.isAbstract(threeArg.modifiers),
            "the mode-aware createSnapshot must stay a default method: pre-mode implementations " +
                "do not implement it",
        )
    }

    @Test
    fun `SnapshotInfo keeps the pre-mode constructor descriptors`() {
        val constructors: Array<Constructor<*>> = SnapshotInfo::class.java.constructors

        assertNotNull(
            constructors.exact(
                String::class.java,
                String::class.java,
                String::class.java,
                SnapshotStatus::class.java,
                OffsetDateTime::class.java,
            ),
            "the five-argument SnapshotInfo constructor is what pre-mode callers bind to",
        )
        assertNotNull(
            constructors.exact(
                String::class.java,
                String::class.java,
                String::class.java,
                SnapshotStatus::class.java,
                OffsetDateTime::class.java,
                Int::class.javaPrimitiveType!!,
                DefaultConstructorMarker::class.java,
            ),
            "the synthetic default-argument constructor is what pre-mode Kotlin callers that omit " +
                "name bind to",
        )
        assertNotNull(
            constructors.exact(
                String::class.java,
                String::class.java,
                String::class.java,
                SnapshotStatus::class.java,
                OffsetDateTime::class.java,
                String::class.java,
            ),
            "the mode-aware six-argument constructor must stay available",
        )
    }

    private fun Array<Method>.exact(
        name: String,
        vararg parameterTypes: Class<*>,
    ): Method? = firstOrNull { it.name == name && it.parameterTypes.contentEquals(parameterTypes) }

    private fun Array<Constructor<*>>.exact(vararg parameterTypes: Class<*>): Constructor<*>? =
        firstOrNull { it.parameterTypes.contentEquals(parameterTypes) }

    /**
     * Matches the compiler-generated static bridge for a facade method with
     * exactly [parameterTypes] plus one defaulted trailing parameter:
     * `$default(instance, params..., int mask, null marker)`.
     */
    private fun Method.isDefaultBridge(
        owner: Class<*>,
        vararg parameterTypes: Class<*>,
    ): Boolean =
        name == "createSnapshot\$default" &&
            Modifier.isStatic(modifiers) &&
            this.parameterTypes.contentEquals(
                arrayOf<Class<*>>(owner) + parameterTypes + Int::class.javaPrimitiveType!! + Any::class.java,
            )
}
