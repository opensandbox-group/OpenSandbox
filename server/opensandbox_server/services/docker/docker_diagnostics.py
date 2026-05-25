# Copyright 2026 Alibaba Group Holding Ltd.
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

"""
Docker diagnostics mixin for DevOps API.

Provides get_sandbox_logs, get_sandbox_inspect, and get_sandbox_events
by reading Docker container state. Mixed into DockerSandboxService.
"""

from __future__ import annotations

import re
import time


def _parse_since_to_timestamp(since: str) -> int:
    """Parse a human-readable duration string (e.g. '10m', '1h') into a Unix timestamp.

    Docker interprets the ``since`` parameter as an absolute Unix timestamp,
    so we convert the relative duration to ``now - duration``.
    """
    m = re.fullmatch(r"(\d+)\s*([smhd])", since.strip().lower())
    if not m:
        seconds = 600  # default 10m
    else:
        value, unit = int(m.group(1)), m.group(2)
        multipliers = {"s": 1, "m": 60, "h": 3600, "d": 86400}
        seconds = value * multipliers[unit]
    return int(time.time()) - seconds


class DockerDiagnosticsMixin:
    """Mixin that implements diagnostics methods for the Docker backend."""

    def get_sandbox_logs(self, sandbox_id: str, tail: int = 100, since: str | None = None) -> str:
        container = self._get_container_by_sandbox_id(sandbox_id)
        kwargs: dict = {"tail": tail, "timestamps": True}
        if since:
            kwargs["since"] = _parse_since_to_timestamp(since)
        output = container.logs(**kwargs)
        if isinstance(output, bytes):
            output = output.decode("utf-8", errors="replace")
        return output or "(no logs)"

    def get_sandbox_inspect(self, sandbox_id: str) -> str:
        container = self._get_container_by_sandbox_id(sandbox_id)
        attrs = container.attrs or {}
        state = attrs.get("State", {})
        config_section = attrs.get("Config", {})
        network_settings = attrs.get("NetworkSettings", {}) or {}
        host_config = attrs.get("HostConfig", {}) or {}

        lines: list[str] = []
        lines.append(f"Container ID:   {container.id[:12]}")
        lines.append(f"Name:           {container.name}")
        lines.append(f"Image:          {config_section.get('Image', 'N/A')}")
        lines.append(f"Status:         {state.get('Status', 'unknown')}")
        lines.append(f"Running:        {state.get('Running', False)}")
        lines.append(f"Paused:         {state.get('Paused', False)}")
        lines.append(f"OOMKilled:      {state.get('OOMKilled', False)}")
        lines.append(f"Exit Code:      {state.get('ExitCode', 'N/A')}")
        lines.append(f"Started At:     {state.get('StartedAt', 'N/A')}")
        lines.append(f"Finished At:    {state.get('FinishedAt', 'N/A')}")
        if state.get("Error"):
            lines.append(f"Error:          {state['Error']}")

        # Resource limits
        lines.append("")
        lines.append("Resources:")
        nano_cpus = host_config.get("NanoCpus", 0)
        if nano_cpus:
            lines.append(f"  CPU:          {nano_cpus / 1e9:.2f} cores")
        memory = host_config.get("Memory", 0)
        if memory:
            lines.append(f"  Memory:       {memory // (1024 * 1024)} MiB")
        pids_limit = host_config.get("PidsLimit", 0)
        if pids_limit:
            lines.append(f"  PIDs Limit:   {pids_limit}")

        # Network
        lines.append("")
        lines.append("Network:")
        networks = network_settings.get("Networks", {})
        for net_name, net_info in networks.items():
            ip = (net_info or {}).get("IPAddress", "N/A")
            lines.append(f"  {net_name}: {ip}")

        # Ports
        ports = network_settings.get("Ports", {})
        if ports:
            lines.append("")
            lines.append("Ports:")
            for port_key, bindings in (ports or {}).items():
                if bindings:
                    for b in bindings:
                        lines.append(f"  {port_key} -> {b.get('HostIp', '')}:{b.get('HostPort', '')}")
                else:
                    lines.append(f"  {port_key} (not bound)")

        # Labels
        labels = config_section.get("Labels", {})
        if labels:
            lines.append("")
            lines.append("Labels:")
            for k, v in sorted(labels.items()):
                lines.append(f"  {k}={v}")

        # Environment (filter sensitive)
        env_list = config_section.get("Env", [])
        if env_list:
            lines.append("")
            lines.append("Environment:")
            for env_entry in env_list:
                key = env_entry.split("=", 1)[0] if "=" in env_entry else env_entry
                if any(s in key.upper() for s in ("SECRET", "TOKEN", "PASSWORD", "KEY")):
                    lines.append(f"  {key}=***")
                else:
                    lines.append(f"  {env_entry}")

        return "\n".join(lines)

    def get_sandbox_events(self, sandbox_id: str, limit: int = 50) -> str:
        container = self._get_container_by_sandbox_id(sandbox_id)
        attrs = container.attrs or {}
        state = attrs.get("State", {})

        lines: list[str] = []

        # Docker does not have a per-container event history in the same way
        # K8s does. We reconstruct key state transitions from container attrs.
        lines.append(f"Container:  {container.id[:12]} ({container.name})")
        lines.append(f"Status:     {state.get('Status', 'unknown')}")

        created = attrs.get("Created", "N/A")
        lines.append(f"Created:    {created}")
        started = state.get("StartedAt")
        if started and started != "0001-01-01T00:00:00Z":
            lines.append(f"Started:    {started}")
        finished = state.get("FinishedAt")
        if finished and finished != "0001-01-01T00:00:00Z":
            lines.append(f"Finished:   {finished}")

        if state.get("OOMKilled"):
            lines.append("Event:      OOMKilled - container was killed due to out-of-memory")
        exit_code = state.get("ExitCode")
        if exit_code is not None and exit_code != 0:
            lines.append(f"Event:      Exited with code {exit_code}")
        if state.get("Error"):
            lines.append(f"Event:      Error - {state['Error']}")

        # Health check status if available
        health = state.get("Health", {})
        if health:
            lines.append(f"Health:     {health.get('Status', 'N/A')}")
            for entry in (health.get("Log") or [])[-limit:]:
                ts = entry.get("Start", "")
                exit_code_h = entry.get("ExitCode", "")
                output_h = (entry.get("Output") or "").strip()[:200]
                lines.append(f"  [{ts}] exit={exit_code_h} {output_h}")

        if len(lines) <= 3:
            lines.append("(no notable events)")

        return "\n".join(lines)
