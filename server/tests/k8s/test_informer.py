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

import threading
import time
from unittest.mock import MagicMock, patch

import pytest
from kubernetes.client import ApiException

from opensandbox_server.services.k8s.informer import WorkloadInformer


def _make_informer(**kwargs) -> WorkloadInformer:
    """Return a WorkloadInformer with a mocked list_fn (watch disabled)."""
    list_fn = kwargs.pop("list_fn", MagicMock(return_value={"items": [], "metadata": {}}))
    enable_watch = kwargs.pop("enable_watch", False)
    return WorkloadInformer(list_fn=list_fn, enable_watch=enable_watch, **kwargs)


def _list_response(*names: str) -> dict:
    """Build a fake CustomObjects list API response."""
    return {
        "metadata": {"resourceVersion": "42"},
        "items": [{"metadata": {"name": n, "resourceVersion": "1"}} for n in names],
    }


class TestWorkloadInformerInit:
    """Construction and property defaults."""

    def test_cache_is_unavailable_before_start(self):
        """Cache reads fall back before the first list completes."""
        informer = _make_informer()
        assert informer.list_if_synced() is None

    def test_get_returns_none_before_sync(self):
        """get() returns None before the cache is populated."""
        informer = _make_informer()
        assert informer.get_if_synced("anything") is None

    def test_resync_and_watch_params_stored(self):
        """Constructor stores resync and watch timeout parameters."""
        informer = _make_informer(resync_period_seconds=120, watch_timeout_seconds=30)
        assert informer.resync_period_seconds == 120
        assert informer.watch_timeout_seconds == 30

class TestWorkloadInformerFullResync:
    """_full_resync populates the cache correctly."""

    def test_full_resync_populates_cache(self):
        """After _full_resync, objects from list_fn are accessible via get()."""
        list_fn = MagicMock(return_value=_list_response("alpha", "beta"))
        informer = _make_informer(list_fn=list_fn)
        assert informer._full_resync() is True

        assert informer.get_if_synced("alpha") is not None
        assert informer.get_if_synced("beta") is not None
        assert informer.get_if_synced("gamma") is None

    def test_full_resync_publishes_cache(self):
        """_full_resync marks the informer as synced."""
        list_fn = MagicMock(return_value=_list_response("x"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()
        assert informer.list_if_synced() is not None

    def test_full_resync_preserves_opaque_resource_version(self):
        """LIST cursor is stored without parsing or comparison."""
        response = _list_response("x")
        response["metadata"]["resourceVersion"] = "rv:abc/7"
        informer = _make_informer(list_fn=MagicMock(return_value=response))

        assert informer._full_resync() is True
        assert informer._resource_version == "rv:abc/7"

    def test_full_resync_replaces_stale_cache(self):
        """A second _full_resync replaces the previous cache contents."""
        list_fn = MagicMock(return_value=_list_response("old"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()
        assert informer.get_if_synced("old") is not None

        list_fn.return_value = _list_response("new")
        informer._full_resync()
        assert informer.get_if_synced("old") is None
        assert informer.get_if_synced("new") is not None

    def test_full_resync_abandons_snapshot_invalidated_during_list(self):
        """An in-flight LIST cannot publish across a completed direct mutation."""
        list_started = threading.Event()
        release_list = threading.Event()

        def list_fn():
            list_started.set()
            assert release_list.wait(timeout=2)
            return _list_response("candidate")

        informer = _make_informer(list_fn=list_fn)
        old = {"metadata": {"name": "old", "resourceVersion": "old-item-rv"}}
        informer._cache = {"old": old}
        informer._resource_version = "old-cursor"
        informer._last_contact_at = 123.0
        informer._has_synced = True
        result = []
        thread = threading.Thread(target=lambda: result.append(informer._full_resync()))

        thread.start()
        assert list_started.wait(timeout=2)
        informer.invalidate()
        release_list.set()
        thread.join(timeout=2)

        assert not thread.is_alive()
        assert result == [False]
        assert informer._cache == {"old": old}
        assert informer._resource_version == "old-cursor"
        assert informer._last_contact_at == 123.0
        assert informer.list_if_synced() is None

        informer.list_fn = MagicMock(return_value=_list_response("recovered"))
        assert informer._full_resync() is True
        assert informer.get_if_synced("old") is None
        assert informer.get_if_synced("recovered") is not None
        assert informer.list_if_synced() is not None

    def test_full_resync_build_failure_does_not_partially_publish(self):
        """Malformed LIST items leave all published state untouched."""
        informer = _make_informer(list_fn=MagicMock(return_value=_list_response("old")))
        assert informer._full_resync() is True
        old_cache = informer._cache
        old_cursor = informer._resource_version
        old_contact = informer._last_contact_at
        informer.list_fn.return_value = {
            "metadata": {"resourceVersion": "new-cursor"},
            "items": [{"metadata": {"name": "new"}}, None],
        }

        with pytest.raises(AttributeError):
            informer._full_resync()

        assert informer._cache is old_cache
        assert informer._resource_version == old_cursor
        assert informer._last_contact_at == old_contact


class TestWorkloadInformerInvalidation:
    """Direct mutations only invalidate the published LIST/WATCH state."""

    def test_invalidate_marks_cache_unsynced_without_changing_cache_or_cursor(self):
        informer = _make_informer(list_fn=MagicMock(return_value=_list_response("foo")))
        assert informer._full_resync() is True
        cached = informer.get_if_synced("foo")
        cursor = informer._resource_version

        informer.invalidate()

        assert informer.list_if_synced() is None
        assert informer._cache["foo"] is cached
        assert informer._resource_version == cursor
        assert informer._invalidation_generation == 1

class TestWorkloadInformerHandleEvent:
    """_handle_event applies watch events to the cache."""

    def test_handle_added_event_inserts_object(self):
        """ADDED event inserts the object into the cache."""
        informer = _make_informer()
        obj = {"metadata": {"name": "bar", "resourceVersion": "10"}}
        informer._handle_event({"type": "ADDED", "object": obj})
        assert informer._cache["bar"] == obj

    def test_handle_modified_event_replaces_object(self):
        """MODIFIED event replaces the cached object."""
        informer = _make_informer()
        informer._cache["bar"] = {"metadata": {"name": "bar", "resourceVersion": "1"}}
        updated = {"metadata": {"name": "bar", "resourceVersion": "2"}}
        informer._handle_event({"type": "MODIFIED", "object": updated})
        assert informer._cache["bar"] == updated

    def test_handle_deleted_event_removes_object(self):
        """DELETED event removes the object from the cache."""
        informer = _make_informer()
        informer._cache["bar"] = {"metadata": {"name": "bar"}}
        informer._handle_event({"type": "DELETED", "object": {"metadata": {"name": "bar"}}})
        assert "bar" not in informer._cache

    @pytest.mark.parametrize("obj", [None, {"metadata": {}}])
    def test_handle_event_ignores_unusable_object(self, obj):
        """Events without a usable named object are ignored."""
        informer = _make_informer()
        informer._handle_event({"type": "ADDED", "object": obj})
        assert informer._cache == {}

    def test_handle_event_converts_non_dict_object(self):
        """Non-dict objects are converted via to_dict() before caching."""
        informer = _make_informer()
        sdk_obj = MagicMock()
        sdk_obj.to_dict.return_value = {"metadata": {"name": "sdk-obj", "resourceVersion": "3"}}
        informer._handle_event({"type": "ADDED", "object": sdk_obj})
        assert "sdk-obj" in informer._cache

    def test_handle_event_updates_resource_version(self):
        """_handle_event advances _resource_version from the object metadata."""
        informer = _make_informer()
        informer._handle_event({
            "type": "ADDED",
            "object": {"metadata": {"name": "foo", "resourceVersion": "77"}},
        })
        assert informer._resource_version == "77"

    def test_handle_event_uses_stream_order_for_opaque_resource_version(self):
        """The last consumed WATCH RV wins without numeric ordering."""
        informer = _make_informer()
        informer._resource_version = "200"
        informer._handle_event({
            "type": "MODIFIED",
            "object": {"metadata": {"name": "foo", "resourceVersion": "rv:abc/7"}},
        })
        assert informer._resource_version == "rv:abc/7"

    def test_handle_event_without_resource_version_requires_resync(self):
        """A cursor-less WATCH event updates internal state but unpublishes it."""
        informer = _make_informer(list_fn=MagicMock(return_value=_list_response("old")))
        assert informer._full_resync() is True
        cursor = informer._resource_version
        updated = {"metadata": {"name": "old", "value": "new"}}

        informer._handle_event({"type": "MODIFIED", "object": updated})

        assert informer._cache["old"] == updated
        assert informer._resource_version == cursor
        assert informer.list_if_synced() is None


class TestWorkloadInformerStaleness:
    """Cache reads reflect whether the informer is still being maintained."""

    def _synced_informer(self) -> WorkloadInformer:
        informer = _make_informer(
            list_fn=MagicMock(return_value=_list_response("x")),
            resync_period_seconds=300,
            watch_timeout_seconds=60,
        )
        informer._full_resync()
        return informer

    def test_cache_unavailable_once_contact_goes_stale(self):
        """A watch that stalls without raising must not leave readers on a frozen cache."""
        informer = self._synced_informer()
        assert informer.list_if_synced() is not None

        informer._last_contact_at -= informer._staleness_limit_seconds + 1
        assert informer.list_if_synced() is None

    def test_cache_available_within_staleness_limit(self):
        """Recent contact keeps the cache usable."""
        informer = self._synced_informer()
        informer._last_contact_at -= informer._staleness_limit_seconds - 1
        assert informer.list_if_synced() is not None

    def test_cache_unavailable_after_stop(self):
        """A stopped informer will never refresh again, however recent its last contact."""
        informer = self._synced_informer()
        informer.stop()
        assert informer.list_if_synced() is None

    def test_completed_watch_stream_refreshes_contact(self):
        """An idle watch that closes cleanly still proves the API server is reachable."""
        informer = self._synced_informer()
        informer._last_contact_at -= informer._staleness_limit_seconds + 1

        fake_watch = MagicMock()
        fake_watch.stream.return_value = iter(())
        with patch(
            "opensandbox_server.services.k8s.informer.watch.Watch",
            return_value=fake_watch,
        ):
            informer._run_watch_loop(60)

        assert informer.list_if_synced() is not None


class TestWorkloadInformerWatchResilience:
    """The watch stream cannot silently park the informer thread."""

    def test_watch_stream_sets_client_side_request_timeout(self):
        """A client read timeout is passed, above the server-side watch timeout."""
        informer = _make_informer()
        fake_watch = MagicMock()
        fake_watch.stream.return_value = iter(())

        with patch(
            "opensandbox_server.services.k8s.informer.watch.Watch",
            return_value=fake_watch,
        ):
            informer._run_watch_loop(60)

        kwargs = fake_watch.stream.call_args.kwargs
        connect_timeout, read_timeout = kwargs["_request_timeout"]
        assert connect_timeout > 0
        assert read_timeout > kwargs["timeout_seconds"]

    def test_raising_watch_stream_does_not_refresh_contact(self):
        """A stream that raises proves nothing about reachability.

        This is the live error path: the client raises ApiException(410) rather
        than yielding an ERROR event, and _run's handler then forces a relist.
        """
        informer = _make_informer(
            list_fn=MagicMock(return_value=_list_response("x")),
            resync_period_seconds=300,
            watch_timeout_seconds=60,
        )
        informer._full_resync()
        informer._last_contact_at -= informer._staleness_limit_seconds + 1

        fake_watch = MagicMock()
        fake_watch.stream.side_effect = ApiException(status=410)

        with patch(
            "opensandbox_server.services.k8s.informer.watch.Watch",
            return_value=fake_watch,
        ):
            with pytest.raises(ApiException):
                informer._run_watch_loop(60)

        assert informer.list_if_synced() is None


class TestWorkloadInformerStartStop:
    """start/stop thread lifecycle."""

    def test_start_launches_daemon_thread(self):
        """start() spawns a daemon thread that is alive."""
        list_fn = MagicMock(return_value={"items": [], "metadata": {}})
        informer = WorkloadInformer(list_fn=list_fn, enable_watch=False,
                                    resync_period_seconds=9999,
                                    thread_name="informer-foos-default")
        informer.start()
        assert informer._thread is not None
        assert informer._thread.is_alive()
        assert informer._thread.name == "informer-foos-default"
        informer.stop()

    def test_start_is_idempotent(self):
        """Calling start() twice does not create a second thread."""
        list_fn = MagicMock(return_value={"items": [], "metadata": {}})
        informer = WorkloadInformer(list_fn=list_fn, enable_watch=False,
                                    resync_period_seconds=9999)
        informer.start()
        first_thread = informer._thread
        informer.start()
        assert informer._thread is first_thread
        informer.stop()

    def test_stop_signals_stop_event(self):
        """stop() sets the internal stop event."""
        informer = _make_informer()
        informer.stop()
        assert informer._stop_event.is_set()

    def test_stopped_informer_cannot_be_restarted(self):
        """start() preserves the terminal stopped state."""
        informer = _make_informer()
        informer.stop()

        informer.start()

        assert informer._thread is None

    def test_resync_conflicts_use_bounded_backoff_before_recovery(self):
        """Repeated invalidation conflicts wait instead of spinning."""
        informer = _make_informer(enable_watch=True)
        informer._full_resync = MagicMock(side_effect=[False, False, True])
        waits = []
        informer._stop_event.wait = MagicMock(side_effect=lambda timeout: waits.append(timeout))

        def stop_after_watch(_timeout_seconds):
            informer.stop()

        informer._run_watch_loop = MagicMock(side_effect=stop_after_watch)

        informer._run()

        assert waits == [1.0, 2.0]
        assert informer._full_resync.call_count == 3
        informer._run_watch_loop.assert_called_once()

    def test_resync_build_error_backs_off_then_recovers(self):
        """A failed candidate build retries and publishes the next valid LIST."""
        list_fn = MagicMock(
            side_effect=[
                {"metadata": {"resourceVersion": "bad"}, "items": [None]},
                _list_response("recovered"),
            ]
        )
        informer = _make_informer(list_fn=list_fn, enable_watch=True)
        waits = []
        informer._stop_event.wait = MagicMock(side_effect=lambda timeout: waits.append(timeout))
        informer._run_watch_loop = MagicMock(side_effect=lambda _timeout: informer.stop())

        informer._run()

        assert waits == [1.0]
        assert list_fn.call_count == 2
        assert "recovered" in informer._cache
        assert informer._resource_version == "42"
        informer._run_watch_loop.assert_called_once()

    def test_stop_interrupts_resync_conflict_backoff(self):
        """The conflict wait uses the stop event and exits without another LIST."""
        informer = _make_informer(enable_watch=True)
        informer._full_resync = MagicMock(return_value=False)

        def stop_during_wait(_timeout):
            informer.stop()
            return True

        informer._stop_event.wait = MagicMock(side_effect=stop_during_wait)

        informer._run()

        informer._stop_event.wait.assert_called_once_with(1.0)
        informer._full_resync.assert_called_once_with()

    def test_poll_mode_resets_has_synced_after_wait(self):
        """In poll mode (enable_watch=False), _has_synced is reset after each wait so the
        cache is refreshed on the next loop iteration."""
        call_count = 0

        def list_fn():
            nonlocal call_count
            call_count += 1
            return {"items": [], "metadata": {"resourceVersion": str(call_count)}}

        informer = WorkloadInformer(
            list_fn=list_fn,
            enable_watch=False,
            resync_period_seconds=0,  # no wait, loop immediately
        )
        informer.start()

        # Give the thread time to execute at least two full loops
        deadline = time.monotonic() + 2.0
        while call_count < 2 and time.monotonic() < deadline:
            time.sleep(0.01)

        informer.stop()
        assert call_count >= 2, "list_fn should be called more than once in poll mode"

    def test_watch_mode_full_resyncs_after_period(self):
        """A normal watch timeout triggers a full list when the resync period elapses."""
        list_fn = MagicMock(
            side_effect=[
                _list_response("stale"),
                _list_response("fresh"),
            ]
        )
        informer = WorkloadInformer(
            list_fn=list_fn,
            enable_watch=True,
            resync_period_seconds=5,
            watch_timeout_seconds=60,
        )
        clock = [0.0]
        watch_timeouts = []
        fake_watch = MagicMock()

        def stream(*args, **kwargs):
            watch_timeouts.append(kwargs["timeout_seconds"])
            clock[0] += kwargs["timeout_seconds"]
            if len(watch_timeouts) == 2:
                informer.stop()
            return iter(())

        fake_watch.stream.side_effect = stream

        with (
            patch("time.monotonic", side_effect=lambda: clock[0]),
            patch(
                "opensandbox_server.services.k8s.informer.watch.Watch",
                return_value=fake_watch,
            ),
        ):
            informer._run()

        assert watch_timeouts == [5, 5]
        assert list_fn.call_count == 2
        assert "stale" not in informer._cache
        assert "fresh" in informer._cache

    def test_watch_mode_does_not_resync_before_period(self):
        """Short watch timeouts resume until the full-resync deadline is reached."""
        list_fn = MagicMock(return_value=_list_response("sandbox"))
        informer = WorkloadInformer(
            list_fn=list_fn,
            enable_watch=True,
            resync_period_seconds=5,
            watch_timeout_seconds=2,
        )
        clock = [0.0]
        list_counts = []
        watch_timeouts = []
        fake_watch = MagicMock()

        def stream(*args, **kwargs):
            watch_timeouts.append(kwargs["timeout_seconds"])
            list_counts.append(list_fn.call_count)
            clock[0] += kwargs["timeout_seconds"]
            if len(watch_timeouts) == 4:
                informer.stop()
            return iter(())

        fake_watch.stream.side_effect = stream

        with (
            patch("time.monotonic", side_effect=lambda: clock[0]),
            patch(
                "opensandbox_server.services.k8s.informer.watch.Watch",
                return_value=fake_watch,
            ),
        ):
            informer._run()

        assert watch_timeouts == [2, 2, 1, 2]
        assert list_counts == [1, 1, 1, 2]
