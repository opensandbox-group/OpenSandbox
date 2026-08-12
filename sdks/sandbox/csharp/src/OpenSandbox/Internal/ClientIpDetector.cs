// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

using System.Net.NetworkInformation;
using System.Net.Sockets;

namespace OpenSandbox.Internal;

/// <summary>
/// Best-effort detection of the SDK host's own intranet IP address.
///
/// The IP is read from the address configured on a real network interface (NOT
/// by dialing out): the outbound route can traverse an egress cluster, whose
/// source IP would not identify the host. A custom (non-standard) header name
/// conveys it because standard forwarded headers such as X-Forwarded-For are
/// rewritten or stripped by intermediaries.
/// </summary>
internal static class ClientIpDetector
{
    // Interface name fragments indicating virtual/container/VPN NICs whose
    // address does not identify the host machine; skipped when picking the IP.
    private static readonly string[] VirtualNicPrefixes =
    {
        "docker", "veth", "br-", "virbr", "vmnet", "vbox",
        "utun", "tun", "tap", "zt", "cni", "flannel", "cali",
    };

    private static readonly object Lock = new();
    private static string? _cached;
    private static volatile Func<string> _detector = DetectOutboundIp;

    /// <summary>
    /// Detection function; overridable for tests. Backed by a volatile field for
    /// cross-thread visibility, mirroring the Kotlin SDK's @Volatile detector.
    /// </summary>
    internal static Func<string> Detector
    {
        get => _detector;
        set => _detector = value;
    }

    /// <summary>Return the intranet IP, detected once and cached for the process.</summary>
    internal static string ClientIp()
    {
        lock (Lock)
        {
            _cached ??= Detector();
            return _cached;
        }
    }

    /// <summary>One interface and its IPv4 addresses; used so selection is unit-testable.</summary>
    internal readonly record struct NicAddrs(string Name, IReadOnlyList<string> Ips);

    /// <summary>Detect the host's intranet IPv4, or empty string if none is found.</summary>
    internal static string DetectOutboundIp()
    {
        try
        {
            var nics = new List<NicAddrs>();
            foreach (var iface in NetworkInterface.GetAllNetworkInterfaces())
            {
                try
                {
                    if (iface.OperationalStatus != OperationalStatus.Up ||
                        iface.NetworkInterfaceType == NetworkInterfaceType.Loopback)
                    {
                        continue;
                    }

                    var ips = new List<string>();
                    foreach (var ua in iface.GetIPProperties().UnicastAddresses)
                    {
                        if (ua.Address.AddressFamily == AddressFamily.InterNetwork)
                        {
                            ips.Add(ua.Address.ToString());
                        }
                    }
                    nics.Add(new NicAddrs(iface.Name, ips));
                }
                catch
                {
                    // Skip this interface; keep enumerating the rest.
                }
            }
            return SelectClientIp(nics);
        }
        catch
        {
            return string.Empty;
        }
    }

    private static bool IsVirtualNic(string name)
    {
        var lower = name.ToLowerInvariant();
        foreach (var p in VirtualNicPrefixes)
        {
            if (lower.StartsWith(p, StringComparison.Ordinal))
            {
                return true;
            }
        }
        return false;
    }

    /// <summary>Rank an interface name by likelihood of being the primary intranet NIC (lower = better).</summary>
    private static int NicNameRank(string name)
    {
        name = name.ToLowerInvariant();
        if (name == "en0")
        {
            return 0;
        }
        if (name == "eth0")
        {
            return 1;
        }
        if (name.StartsWith("eth", StringComparison.Ordinal))
        {
            return 2;
        }
        if (name.StartsWith("en", StringComparison.Ordinal)) // covers en*, ens*, enp*, eno*
        {
            return 3;
        }
        if (name.StartsWith("bond", StringComparison.Ordinal))
        {
            return 4;
        }
        return 100;
    }

    private static int[]? ParseIpv4(string ip)
    {
        var parts = ip.Split('.');
        if (parts.Length != 4)
        {
            return null;
        }
        var nums = new int[4];
        for (var i = 0; i < 4; i++)
        {
            if (!int.TryParse(parts[i], out var n) || n < 0 || n > 255)
            {
                return null;
            }
            nums[i] = n;
        }
        return nums;
    }

    private static bool IsUsableIpv4(string ip)
    {
        var o = ParseIpv4(ip);
        if (o == null)
        {
            return false;
        }
        if (o[0] == 127) // loopback
        {
            return false;
        }
        if (o[0] == 169 && o[1] == 254) // link-local
        {
            return false;
        }
        if (o[0] == 0 && o[1] == 0 && o[2] == 0 && o[3] == 0) // unspecified
        {
            return false;
        }
        return true;
    }

    private static bool IsPrivateIpv4(string ip)
    {
        var o = ParseIpv4(ip);
        if (o == null)
        {
            return false;
        }
        if (o[0] == 10)
        {
            return true;
        }
        if (o[0] == 172 && o[1] >= 16 && o[1] <= 31)
        {
            return true;
        }
        if (o[0] == 192 && o[1] == 168)
        {
            return true;
        }
        return false;
    }

    /// <summary>
    /// Pick the host's intranet IPv4 from the given interfaces. Prefers, in order:
    /// known NIC names (<see cref="NicNameRank"/>), then RFC1918 private addresses
    /// — network-name priority with private-segment fallback in a single pass.
    /// Virtual/container/VPN NICs are skipped. Returns empty string if none is usable.
    /// </summary>
    internal static string SelectClientIp(IReadOnlyList<NicAddrs> nics)
    {
        var best = string.Empty;
        var bestRank = 0;
        var bestPrivate = false;
        var found = false;

        foreach (var nic in nics)
        {
            if (IsVirtualNic(nic.Name))
            {
                continue;
            }
            var rank = NicNameRank(nic.Name);
            foreach (var ip in nic.Ips)
            {
                if (!IsUsableIpv4(ip))
                {
                    continue;
                }
                var priv = IsPrivateIpv4(ip);
                if (!found || rank < bestRank || (rank == bestRank && priv && !bestPrivate))
                {
                    best = ip;
                    bestRank = rank;
                    bestPrivate = priv;
                    found = true;
                }
            }
        }
        return best;
    }

    /// <summary>Reset detection state. Intended for tests only.</summary>
    internal static void ResetForTest()
    {
        lock (Lock)
        {
            _cached = null;
            Detector = DetectOutboundIp;
        }
    }
}
