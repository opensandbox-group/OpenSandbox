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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceResult
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.EntryInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.MoveEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SearchEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SetPermissionEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import java.io.InputStream

class FilesystemCompatibilityTest {
    @Test
    fun `legacy adapters work without implementing download metadata`() {
        val files: Filesystem = LegacyFilesystem()

        assertArrayEquals(byteArrayOf(1, 2), files.readByteArray("/data.bin"))
        assertThrows<UnsupportedOperationException> { files.readByteArrayDetailed("/data.bin") }
        assertThrows<UnsupportedOperationException> { files.readStreamDetailed("/data.bin") }
    }

    private class LegacyFilesystem : Filesystem {
        override fun readFile(
            path: String,
            encoding: String,
            range: String?,
            offset: Int?,
            limit: Int?,
        ): String = unsupported()

        override fun readByteArray(
            path: String,
            range: String?,
            offset: Int?,
            limit: Int?,
        ): ByteArray = byteArrayOf(1, 2)

        override fun readStream(
            path: String,
            range: String?,
            offset: Int?,
            limit: Int?,
        ): InputStream = unsupported()

        override fun write(entries: List<WriteEntry>) = unsupported()

        override fun createDirectories(entries: List<WriteEntry>) = unsupported()

        override fun deleteFiles(paths: List<String>) = unsupported()

        override fun deleteDirectories(paths: List<String>) = unsupported()

        override fun listDirectory(
            path: String,
            depth: Int?,
        ): List<EntryInfo> = unsupported()

        override fun moveFiles(entries: List<MoveEntry>) = unsupported()

        override fun setPermissions(entries: List<SetPermissionEntry>) = unsupported()

        override fun replaceContents(entries: List<ContentReplaceEntry>) = unsupported()

        override fun replaceContentsDetailed(entries: List<ContentReplaceEntry>): List<ContentReplaceResult> = unsupported()

        override fun search(entry: SearchEntry): List<EntryInfo> = unsupported()

        override fun readFileInfo(paths: List<String>): Map<String, EntryInfo> = unsupported()

        private fun unsupported(): Nothing = throw UnsupportedOperationException("not used")
    }
}
