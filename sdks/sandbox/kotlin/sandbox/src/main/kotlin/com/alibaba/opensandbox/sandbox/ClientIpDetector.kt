/*
 * Copyright 2026 Alibaba Group Holding Ltd.
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

import java.net.Inet4Address
import java.net.NetworkInterface

/**
 * Best-effort detection of the SDK host's own intranet IP address.
 *
 * The IP is read from the address configured on a real network interface (NOT by
 * dialing out): the outbound route can traverse an egress cluster, whose source
 * IP would not identify the host. The detected value is sent to the server via
 * [CLIENT_IP_HEADER]. A custom (non-standard) header name is used on purpose:
 * standard forwarded headers such as X-Forwarded-For are rewritten or stripped
 * by intermediaries.
 */
internal object ClientIpDetector {
    const val CLIENT_IP_HEADER = "OPEN-SANDBOX-CLIENT-IP"

    /**
     * Interface name fragments indicating virtual/container/VPN NICs whose
     * address does not identify the host machine; skipped when picking the IP.
     */
    private val VIRTUAL_NIC_PREFIXES =
        listOf(
            "docker", "veth", "br-", "virbr", "vmnet", "vbox",
            "utun", "tun", "tap", "zt", "cni", "flannel", "cali",
        )

    /** Detection function; overridable for tests. */
    @Volatile
    internal var detector: () -> String = ::detectOutboundIp

    @Volatile
    private var cached: String? = null

    /** Return the intranet IP, detected once and cached for the process. */
    fun clientIp(): String {
        // Fast path: lock-free read of the volatile cache on the hot request path.
        cached?.let { return it }
        return synchronized(this) {
            cached ?: detector().also { cached = it }
        }
    }

    /** One interface and its IPv4 addresses; used so selection is unit-testable. */
    internal data class NicAddrs(val name: String, val ips: List<String>)

    /** Detect the host's intranet IPv4, or "" if none is found. */
    fun detectOutboundIp(): String {
        return try {
            val nics = mutableListOf<NicAddrs>()
            val ifaces = NetworkInterface.getNetworkInterfaces() ?: return ""
            for (iface in ifaces) {
                try {
                    if (!iface.isUp || iface.isLoopback) continue
                    val ips =
                        iface.inetAddresses.toList()
                            .filterIsInstance<Inet4Address>()
                            .mapNotNull { it.hostAddress }
                    nics.add(NicAddrs(iface.name, ips))
                } catch (_: Exception) {
                    // Skip this interface; keep enumerating the rest.
                }
            }
            selectClientIp(nics)
        } catch (_: Exception) {
            ""
        }
    }

    private fun isVirtualNic(name: String): Boolean {
        val lower = name.lowercase()
        return VIRTUAL_NIC_PREFIXES.any { lower.startsWith(it) }
    }

    /** Rank an interface name by likelihood of being the primary intranet NIC (lower = better). */
    private fun nicNameRank(rawName: String): Int {
        val name = rawName.lowercase()
        return when {
            name == "en0" -> 0
            name == "eth0" -> 1
            name.startsWith("eth") -> 2
            name.startsWith("en") -> 3 // covers en*, ens*, enp*, eno*
            name.startsWith("bond") -> 4
            else -> 100
        }
    }

    private fun parseIpv4(ip: String): IntArray? {
        val parts = ip.split(".")
        if (parts.size != 4) return null
        val nums = IntArray(4)
        for (i in 0 until 4) {
            val n = parts[i].toIntOrNull() ?: return null
            if (n < 0 || n > 255) return null
            nums[i] = n
        }
        return nums
    }

    private fun isUsableIpv4(ip: String): Boolean {
        val o = parseIpv4(ip) ?: return false
        if (o[0] == 127) return false // loopback
        if (o[0] == 169 && o[1] == 254) return false // link-local
        if (o[0] == 0 && o[1] == 0 && o[2] == 0 && o[3] == 0) return false // unspecified
        return true
    }

    private fun isPrivateIpv4(ip: String): Boolean {
        val o = parseIpv4(ip) ?: return false
        return when {
            o[0] == 10 -> true
            o[0] == 172 && o[1] in 16..31 -> true
            o[0] == 192 && o[1] == 168 -> true
            else -> false
        }
    }

    /**
     * Pick the host's intranet IPv4 from the given interfaces. Prefers, in order:
     * known NIC names ([nicNameRank]), then RFC1918 private addresses — i.e.
     * network-name priority with private-segment fallback in a single pass.
     * Virtual/container/VPN NICs are skipped. Returns "" if none is usable.
     */
    internal fun selectClientIp(nics: List<NicAddrs>): String {
        var best = ""
        var bestRank = 0
        var bestPrivate = false
        var found = false

        for (nic in nics) {
            if (isVirtualNic(nic.name)) continue
            val rank = nicNameRank(nic.name)
            for (ip in nic.ips) {
                if (!isUsableIpv4(ip)) continue
                val priv = isPrivateIpv4(ip)
                if (!found || rank < bestRank || (rank == bestRank && priv && !bestPrivate)) {
                    best = ip
                    bestRank = rank
                    bestPrivate = priv
                    found = true
                }
            }
        }
        return best
    }

    /** Reset detection state. Intended for tests only. */
    @Synchronized
    internal fun resetForTest() {
        cached = null
        detector = ::detectOutboundIp
    }
}
