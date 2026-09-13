"""Opt-in regressions through DELETE/TTL against a real egress image.

Run with DOCKER_HOST pointing to the test daemon, OPENSANDBOX_TEST_DOCKER=1
and OPENSANDBOX_TEST_EGRESS_IMAGE set to an image built from current source.
Only fixture-owned containers and volumes are changed.
"""
import json
import os
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from unittest.mock import patch
from uuid import uuid4

import docker
import pytest
from docker.errors import APIError, NotFound
from fastapi import HTTPException

from opensandbox_server.config import AppConfig, RuntimeConfig
from opensandbox_server.services.constants import SANDBOX_ID_LABEL, SANDBOX_MANAGED_VOLUMES_LABEL
from opensandbox_server.services.docker import DockerSandboxService
from opensandbox_server.services.docker.metadata import DockerMetadataStore
from opensandbox_server.services.docker.networking import EGRESS_SIDECAR_LABEL

pytestmark = pytest.mark.skipif(os.environ.get("OPENSANDBOX_TEST_DOCKER") != "1",
                                reason="requires explicit real Docker opt-in")


def read_json(url):
    with urllib.request.urlopen(url, timeout=1) as response:
        return json.load(response)


def wait_http(url):
    deadline = time.monotonic() + 15
    while True:
        try:
            with urllib.request.urlopen(url, timeout=1) as response:
                assert response.status == 200
                return
        except (urllib.error.URLError, TimeoutError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.05)


@pytest.fixture
def sandbox_pair(tmp_path, request):
    client = docker.DockerClient.from_env()
    with patch("docker.from_env", return_value=client), patch.object(client.containers, "list", return_value=[]):
        service = DockerSandboxService(config=AppConfig(runtime=RuntimeConfig(type="docker", execd_image="unused")))
    service._metadata_store = DockerMetadataStore(tmp_path / "metadata")
    sandbox_id = f"delete-regression-{uuid4().hex}"
    containers = []
    volume = client.volumes.create(name=f"opensandbox-runtime-{sandbox_id}",
                                  labels={SANDBOX_MANAGED_VOLUMES_LABEL: "server"})
    mount = {volume.name: {"bind": "/test-runtime", "mode": "rw"}}
    mode = getattr(request, "param", 0)
    delay = mode if isinstance(mode, int) else 0
    response_status = 500 if mode == "retry" else 200
    try:
        collector = client.containers.run("node:24-bookworm-slim", ["node", "-e", f"""
const events = [];
require('http').createServer((req, res) => {{
  if (req.method === 'GET') {{ res.end(JSON.stringify(events)); return; }}
  let body = '';
  req.on('data', chunk => body += chunk);
  req.on('end', () => {{
    const event = JSON.parse(body); event.acknowledged = false; events.push(event);
    setTimeout(() => {{ event.acknowledged = true; res.statusCode = {response_status}; res.end('ok'); }}, {delay});
  }});
}}).listen(18081, '0.0.0.0');
"""], detach=True, ports={"18081/tcp": ("127.0.0.1", 0)})
        containers.append(collector)
        collector.reload()
        ip = collector.attrs["NetworkSettings"]["Networks"]["bridge"]["IPAddress"]
        host_port = collector.attrs["NetworkSettings"]["Ports"]["18081/tcp"][0]["HostPort"]
        collector_url = f"http://127.0.0.1:{host_port}"
        wait_http(collector_url)
        sidecar = client.containers.run(
            os.environ.get("OPENSANDBOX_TEST_EGRESS_IMAGE", "opensandbox/egress:v1.1.7"),
            detach=True, cap_add=["NET_ADMIN"], ports={"18080/tcp": ("127.0.0.1", 0)},
            labels={EGRESS_SIDECAR_LABEL: sandbox_id}, volumes=mount,
            environment={"OPENSANDBOX_EGRESS_MODE": "dns", "OPENSANDBOX_EGRESS_SANDBOX_ID": sandbox_id,
                         "OPENSANDBOX_EGRESS_RULES": json.dumps({"defaultAction": "allow", "egress": [
                             {"action": "deny", "target": "*.blocked.invalid"}]}),
                         "OPENSANDBOX_EGRESS_DENY_WEBHOOK": f"http://{ip}:18081"},
        )
        containers.append(sidecar)
        sidecar.reload()
        port = sidecar.attrs["NetworkSettings"]["Ports"]["18080/tcp"][0]["HostPort"]
        wait_http(f"http://127.0.0.1:{port}/healthz")
        app = client.containers.run("node:24-bookworm-slim", ["sleep", "600"], detach=True,
                                    name=f"sandbox-{sandbox_id}", network_mode=f"container:{sidecar.id}",
                                    volumes=mount, labels={SANDBOX_ID_LABEL: sandbox_id,
                                    SANDBOX_MANAGED_VOLUMES_LABEL: json.dumps([volume.name])})
        containers.append(app)
        yield service, client, sandbox_id, app, sidecar, collector_url, volume
    finally:
        for timer in list(service._expiration_timers.values()):
            timer.cancel()
        for container in reversed(containers):
            try:
                container.remove(force=True)
            except NotFound:
                pass
        try:
            volume.remove()
        except NotFound:
            pass
        client.close()


def emit_blocked(app, count=3):
    result = app.exec_run(["node", "-e", f"""
const {{Resolver}} = require('node:dns').promises;
const resolver = new Resolver({{timeout:1000, tries:1}});
resolver.setServers(['127.0.0.1:15353']);
Promise.all(Array.from({{length:{count}}}, (_, i) =>
  resolver.resolve4(i+'.blocked.invalid').then(() => {{throw Error('not denied')}},
    e => {{if (e.code !== 'ENOTFOUND') throw e}}))).catch(e=>{{console.error(e);process.exitCode=1}});
"""])
    assert result.exit_code == 0, result.output


def assert_removed(client, app, sidecar, volume):
    for container in (app, sidecar):
        with pytest.raises(NotFound):
            client.containers.get(container.id)
    with pytest.raises(NotFound):
        client.volumes.get(volume.name)


@pytest.mark.parametrize("sandbox_pair", [75], indirect=True)
def test_delete_gives_queued_webhooks_a_short_drain(sandbox_pair):
    service, client, sandbox_id, app, sidecar, url, volume = sandbox_pair
    emit_blocked(app)
    started = time.monotonic()
    service.delete_sandbox(sandbox_id)
    elapsed = time.monotonic() - started
    received = read_json(url)
    assert len(received) == 3 and all(event["acknowledged"] for event in received)
    assert elapsed < 2.5
    assert_removed(client, app, sidecar, volume)


@pytest.mark.parametrize("operation", ["kill", "remove_container"])
def test_delete_failure_preserves_dependencies(sandbox_pair, operation):
    service, client, sandbox_id, app, sidecar, _, volume = sandbox_pair
    expiration = datetime.now(timezone.utc) + timedelta(hours=1)
    service._metadata_store.set_expiration(sandbox_id, expiration)
    with patch.object(client.api, operation, side_effect=APIError("daemon failure")):
        with pytest.raises(HTTPException) as error:
            service.delete_sandbox(sandbox_id)
    assert error.value.status_code == 500
    app.reload()
    sidecar.reload()
    assert sidecar.status == "running"
    client.volumes.get(volume.name)
    assert service._metadata_store.get_expiration(sandbox_id) == expiration.isoformat()
    service.delete_sandbox(sandbox_id)
    assert_removed(client, app, sidecar, volume)


def test_ttl_remove_failure_retains_resources_and_retries(sandbox_pair):
    from threading import Timer

    service, client, sandbox_id, app, sidecar, _, volume = sandbox_pair
    expiration = datetime.now(timezone.utc) - timedelta(seconds=1)
    service._metadata_store.set_expiration(sandbox_id, expiration)
    scheduled = []

    def timer_factory(*args, **kwargs):
        timer = Timer(*args, **kwargs)
        timer.start = lambda: None
        scheduled.append(timer)
        return timer

    original_remove = client.api.remove_container

    def fail_app_remove(container, **kwargs):
        if container == app.id:
            raise APIError("application removal failed")
        return original_remove(container, **kwargs)

    with patch("opensandbox_server.services.docker.docker_service.Timer", side_effect=timer_factory), patch.object(
        client.api, "remove_container", side_effect=fail_app_remove
    ):
        service._expire_sandbox(sandbox_id)
    assert len(scheduled) == 1
    sidecar.reload()
    assert sidecar.status == "running"
    assert service._metadata_store.get_expiration(sandbox_id) == expiration.isoformat()
    scheduled[0].function(*scheduled[0].args, **scheduled[0].kwargs)
    assert_removed(client, app, sidecar, volume)


@pytest.mark.parametrize("retry_by", ["delete", "ttl"])
def test_missing_application_still_allows_sidecar_cleanup(sandbox_pair, retry_by):
    service, client, sandbox_id, app, sidecar, _, volume = sandbox_pair
    original_remove = client.api.remove_container

    def fail_sidecar_remove(container, **kwargs):
        if container == sidecar.id:
            raise APIError("sidecar removal failed")
        return original_remove(container, **kwargs)

    with patch.object(client.api, "remove_container", side_effect=fail_sidecar_remove):
        service.delete_sandbox(sandbox_id)
    sidecar.reload()
    assert sidecar.status == "exited"
    if retry_by == "delete":
        with pytest.raises(HTTPException) as error:
            service.delete_sandbox(sandbox_id)
        assert error.value.status_code == 404
    else:
        service._expire_sandbox(sandbox_id)
    assert_removed(client, app, sidecar, volume)


@pytest.mark.parametrize("sandbox_pair", [12000, "retry", "worker_stuck", "supervisor_stuck"], indirect=True)
def test_slow_or_stuck_egress_cannot_extend_stop_grace(sandbox_pair, request):
    from threading import Thread

    service, client, sandbox_id, app, sidecar, url, volume = sandbox_pair
    mode = request.node.callspec.params["sandbox_pair"]
    stream = sidecar.logs(stream=True, follow=True)
    logs = []
    reader = Thread(target=lambda: logs.extend(stream), daemon=True)
    reader.start()
    try:
        if mode == "worker_stuck":
            result = sidecar.exec_run(["pkill", "-STOP", "-x", "egress"])
            assert result.exit_code == 0, result.output
        elif mode == "supervisor_stuck":
            sidecar.kill(signal="SIGSTOP")
        else:
            emit_blocked(app, 1)
            deadline = time.monotonic() + 2
            while not read_json(url):
                assert time.monotonic() < deadline, "webhook was not received"
                time.sleep(0.01)
        started = time.monotonic()
        service.delete_sandbox(sandbox_id)
        elapsed = time.monotonic() - started
        print(f"mode={mode} DELETE={elapsed:.3f}s")
        assert elapsed < 10.5  # Includes daemon/volume overhead beyond the 9s stop grace.
        assert_removed(client, app, sidecar, volume)
        reader.join(timeout=2)
        if mode in (12000, "retry"):
            assert "drain timed out" in b"".join(logs).decode(errors="replace")
        else:
            # Both stalls outlast the Docker stop grace; the supervisor allows 20s.
            assert elapsed >= 8.5
    finally:
        stream.close()


def test_stop_transport_error_still_forces_removal(sandbox_pair):
    from requests.exceptions import ReadTimeout

    service, client, sandbox_id, app, sidecar, _, volume = sandbox_pair
    with patch.object(client.api, "stop", side_effect=ReadTimeout("lost stop response")):
        service.delete_sandbox(sandbox_id)
    assert_removed(client, app, sidecar, volume)


@pytest.mark.parametrize("sandbox_pair", [2000], indirect=True)
def test_repeated_delete_during_stop_preserves_404(sandbox_pair):
    from concurrent.futures import ThreadPoolExecutor
    from threading import Event

    service, client, sandbox_id, app, sidecar, _, volume = sandbox_pair
    emit_blocked(app, 1)
    stopping = Event()
    original_stop = client.api.stop

    def observed_stop(*args, **kwargs):
        stopping.set()
        return original_stop(*args, **kwargs)

    with patch.object(client.api, "stop", side_effect=observed_stop), ThreadPoolExecutor(2) as executor:
        first = executor.submit(service.delete_sandbox, sandbox_id)
        assert stopping.wait(2)
        second = executor.submit(service.delete_sandbox, sandbox_id)
        first.result(timeout=5)
        with pytest.raises(HTTPException) as error:
            second.result(timeout=5)
        assert error.value.status_code == 404
    assert_removed(client, app, sidecar, volume)


@pytest.mark.parametrize("sandbox_pair", [200], indirect=True)
def test_delete_drains_a_small_backlog_before_forcing_exit(sandbox_pair):
    service, client, sandbox_id, app, sidecar, url, volume = sandbox_pair
    emit_blocked(app, 20)
    started = time.monotonic()
    service.delete_sandbox(sandbox_id)
    received = read_json(url)
    assert len(received) == 20 and all(event["acknowledged"] for event in received)
    assert time.monotonic() - started < 6.5
    assert_removed(client, app, sidecar, volume)
