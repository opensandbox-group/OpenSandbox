#
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""
Best-effort detection of the SDK host's own intranet IP address.

The detected IP is read from a real network interface (NOT by dialing out): the
outbound route can traverse an egress cluster, whose source IP would not
identify the host. The value is sent to the server via a custom header. A custom
(non-standard) header name is used on purpose: standard forwarded headers such
as X-Forwarded-For are rewritten or stripped by intermediaries, so a dedicated
name conveys the value reliably.

Interfaces are enumerated with the libc ``getifaddrs(3)`` call via ``ctypes``
(no third-party dependency). This works uniformly on Linux and macOS/BSD, is
non-blocking, and does no DNS lookup. Windows (no ``getifaddrs``) degrades to
returning "".
"""

import ctypes
import ipaddress
import socket
import sys

CLIENT_IP_HEADER = "OPEN-SANDBOX-CLIENT-IP"

# Interface name fragments indicating virtual/container/VPN NICs whose address
# does not identify the host machine; skipped when picking the host's own IP.
_VIRTUAL_NIC_PREFIXES = (
    "docker",
    "veth",
    "br-",
    "virbr",
    "vmnet",
    "vbox",
    "utun",
    "tun",
    "tap",
    "zt",
    "cni",
    "flannel",
    "cali",
)

# Interface flags (same values on Linux and macOS/BSD).
_IFF_UP = 0x1
_IFF_LOOPBACK = 0x8

# macOS/BSD put a 1-byte sa_len before sa_family; Linux starts with a 2-byte
# sa_family. This changes where the address family byte lives in a sockaddr.
_IS_BSD = sys.platform == "darwin" or "bsd" in sys.platform

_cached_ip: str | None = None


class _Ifaddrs(ctypes.Structure):
    """Mirror of ``struct ifaddrs``; only the fields we read are declared.

    The leading layout (next, name, flags, addr) is identical on Linux and
    macOS/BSD, which is why getifaddrs unifies both platforms.
    """


_Ifaddrs._fields_ = [
    ("ifa_next", ctypes.POINTER(_Ifaddrs)),
    ("ifa_name", ctypes.c_char_p),
    ("ifa_flags", ctypes.c_uint),
    ("ifa_addr", ctypes.POINTER(ctypes.c_ubyte * 16)),
    ("ifa_netmask", ctypes.POINTER(ctypes.c_ubyte * 16)),
    ("ifa_dstaddr", ctypes.POINTER(ctypes.c_ubyte * 16)),
    ("ifa_data", ctypes.c_void_p),
]


def _is_virtual_nic(name: str) -> bool:
    lower = name.lower()
    return any(lower.startswith(p) for p in _VIRTUAL_NIC_PREFIXES)


def _nic_name_rank(name: str) -> int:
    """Rank an interface name by likelihood of being the primary intranet NIC.

    Lower is better; named NICs beat unknown ones.
    """
    name = name.lower()
    if name == "en0":
        return 0
    if name == "eth0":
        return 1
    if name.startswith("eth"):
        return 2
    if name.startswith("en"):  # covers en*, ens*, enp*, eno*
        return 3
    if name.startswith("bond"):
        return 4
    return 100


def _usable_ipv4(ip: str) -> ipaddress.IPv4Address | None:
    try:
        addr = ipaddress.ip_address(ip)
    except ValueError:
        return None
    if not isinstance(addr, ipaddress.IPv4Address):
        return None
    if addr.is_loopback or addr.is_link_local or addr.is_unspecified:
        return None
    return addr


def _select_client_ip(nics: list[tuple[str, list[str]]]) -> str:
    """Pick the host's intranet IPv4 from ``(name, ips)`` interface entries.

    Selection prefers, in order: known NIC names (:func:`_nic_name_rank`), then
    RFC1918 private addresses — i.e. network-name priority with private-segment
    fallback in a single pass. Virtual/container/VPN NICs are skipped. Returns
    "" if no usable address exists.
    """
    best = ""
    best_rank = 0
    best_private = False
    found = False

    for name, ips in nics:
        if _is_virtual_nic(name):
            continue
        rank = _nic_name_rank(name)
        for ip in ips:
            addr = _usable_ipv4(ip)
            if addr is None:
                continue
            private = addr.is_private
            if (
                not found
                or rank < best_rank
                or (rank == best_rank and private and not best_private)
            ):
                best, best_rank, best_private, found = ip, rank, private, True

    return best


def _sockaddr_ipv4(addr_ptr: "ctypes._Pointer[ctypes.Array[ctypes.c_ubyte]]") -> str | None:
    """Return the dotted IPv4 string from a sockaddr pointer, or None.

    ``sin_addr`` sits at offset 4 in ``sockaddr_in`` on both Linux and
    macOS/BSD (family/len bytes + 2-byte port precede it), so only the family
    byte position differs between platforms.
    """
    if not addr_ptr:
        return None
    # sockaddr is >= 16 bytes on both OSes; read the fixed header safely.
    raw = ctypes.string_at(ctypes.cast(addr_ptr, ctypes.c_void_p), 16)
    family = raw[1] if _IS_BSD else raw[0]
    if family != socket.AF_INET:
        return None
    return socket.inet_ntoa(raw[4:8])


def _enumerate_nics() -> list[tuple[str, list[str]]]:
    """Enumerate (name, [ipv4]) via libc getifaddrs. Returns [] if unavailable."""
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        getifaddrs = libc.getifaddrs
        freeifaddrs = libc.freeifaddrs
    except (OSError, AttributeError):
        # No libc getifaddrs (e.g. Windows): degrade to empty.
        return []

    getifaddrs.restype = ctypes.c_int
    getifaddrs.argtypes = [ctypes.POINTER(ctypes.POINTER(_Ifaddrs))]
    freeifaddrs.argtypes = [ctypes.POINTER(_Ifaddrs)]

    head = ctypes.POINTER(_Ifaddrs)()
    if getifaddrs(ctypes.byref(head)) != 0:
        return []

    by_name: dict[str, list[str]] = {}
    order: list[str] = []
    try:
        node = head
        while node:
            ifa = node.contents
            flags = ifa.ifa_flags
            if (flags & _IFF_UP) and not (flags & _IFF_LOOPBACK):
                ip = _sockaddr_ipv4(ifa.ifa_addr)
                if ip:
                    name = ifa.ifa_name.decode() if ifa.ifa_name else ""
                    if name not in by_name:
                        by_name[name] = []
                        order.append(name)
                    by_name[name].append(ip)
            node = ifa.ifa_next
    finally:
        freeifaddrs(head)

    return [(name, by_name[name]) for name in order]


def detect_outbound_ip() -> str:
    """Return the host's intranet IPv4, or "" if it cannot be determined.

    Best-effort: any failure yields "" and never propagates (config init must
    not break).
    """
    try:
        return _select_client_ip(_enumerate_nics())
    except Exception:
        return ""


def get_client_ip() -> str:
    """Return the intranet IP, detected once and cached for the process."""
    global _cached_ip
    if _cached_ip is None:
        _cached_ip = detect_outbound_ip()
    return _cached_ip


def apply_client_ip(headers: dict[str, str]) -> dict[str, str]:
    """Add the client IP header in place unless already present or undetectable.

    An existing header (matched case-insensitively) is left untouched so a
    user-supplied value is never overwritten.
    """
    ip = get_client_ip()
    if not ip:
        return headers
    if any(k.lower() == CLIENT_IP_HEADER.lower() for k in headers):
        return headers
    headers[CLIENT_IP_HEADER] = ip
    return headers
