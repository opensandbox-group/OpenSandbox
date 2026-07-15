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

import time
from unittest.mock import MagicMock

from opensandbox_server.services.k8s.informer import WorkloadInformer


def _make_informer(**kwargs) -> WorkloadInformer:
    """Return a WorkloadInformer with a mocked list_fn (watch disabled)."""
    list_fn = kwargs.pop("list_fn", MagicMock(return_value={"items": [], "metadata": {}}))
    return WorkloadInformer(list_fn=list_fn, enable_watch=False, **kwargs)


def _list_response(*names: str) -> dict:
    """Build a fake CustomObjects list API response."""
    return {
        "metadata": {"resourceVersion": "42"},
        "items": [{"metadata": {"name": n, "resourceVersion": "1"}} for n in names],
    }


class TestWorkloadInformerInit:
    """Construction and property defaults."""

    def test_has_synced_is_false_before_start(self):
        """has_synced starts as False before the first list completes."""
        informer = _make_informer()
        assert informer.has_synced is False

    def test_get_returns_none_before_sync(self):
        """get() returns None before the cache is populated."""
        informer = _make_informer()
        assert informer.get("anything") is None

    def test_resync_and_watch_params_stored(self):
        """Constructor stores resync and watch timeout parameters."""
        informer = _make_informer(resync_period_seconds=120, watch_timeout_seconds=30)
        assert informer.resync_period_seconds == 120
        assert informer.watch_timeout_seconds == 30

    def test_custom_thread_name_is_stored(self):
        """thread_name parameter is stored and used when start() is called."""
        informer = _make_informer(thread_name="informer-foos-default")
        assert informer._thread_name == "informer-foos-default"

    def test_default_thread_name(self):
        """Default thread_name is 'workload-informer' when not specified."""
        informer = _make_informer()
        assert informer._thread_name == "workload-informer"


class TestWorkloadInformerFullResync:
    """_full_resync populates the cache correctly."""

    def test_full_resync_populates_cache(self):
        """After _full_resync, objects from list_fn are accessible via get()."""
        list_fn = MagicMock(return_value=_list_response("alpha", "beta"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()

        assert informer.get("alpha") is not None
        assert informer.get("beta") is not None
        assert informer.get("gamma") is None

    def test_full_resync_sets_has_synced(self):
        """_full_resync marks the informer as synced."""
        list_fn = MagicMock(return_value=_list_response("x"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()
        assert informer.has_synced is True

    def test_full_resync_stores_resource_version(self):
        """_full_resync saves the resourceVersion from the list metadata."""
        list_fn = MagicMock(return_value=_list_response("x"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()
        assert informer._resource_version == "42"

    def test_full_resync_replaces_stale_cache(self):
        """A second _full_resync replaces the previous cache contents."""
        list_fn = MagicMock(return_value=_list_response("old"))
        informer = _make_informer(list_fn=list_fn)
        informer._full_resync()
        assert informer.get("old") is not None

        list_fn.return_value = _list_response("new")
        informer._full_resync()
        assert informer.get("old") is None
        assert informer.get("new") is not None


class TestWorkloadInformerUpdateCache:
    """update_cache upserts objects into the cache."""

    def test_update_cache_adds_new_object(self):
        """update_cache makes a previously missing object retrievable."""
        informer = _make_informer()
        obj = {"metadata": {"name": "foo", "resourceVersion": "5"}}
        informer.update_cache(obj)
        assert informer.get("foo") == obj

    def test_update_cache_overwrites_existing_object(self):
        """update_cache replaces the cached version of an object."""
        informer = _make_informer()
        informer.update_cache({"metadata": {"name": "foo", "resourceVersion": "1"}})
        updated = {"metadata": {"name": "foo", "resourceVersion": "2"}}
        informer.update_cache(updated)
        assert informer.get("foo") == updated

    def test_update_cache_ignores_object_without_name(self):
        """update_cache silently ignores objects that lack a metadata.name."""
        informer = _make_informer()
        informer.update_cache({"metadata": {}})
        # Cache remains empty — no exception raised
        assert informer._cache == {}

    def test_update_cache_updates_resource_version(self):
        """update_cache advances _resource_version from object metadata."""
        informer = _make_informer()
        informer.update_cache({"metadata": {"name": "foo", "resourceVersion": "99"}})
        assert informer._resource_version == "99"

    def test_update_cache_does_not_downgrade_resource_version(self):
        """update_cache never rolls back _resource_version to an older value."""
        informer = _make_informer()
        informer._resource_version = "200"
        informer.update_cache({"metadata": {"name": "foo", "resourceVersion": "100"}})
        assert informer._resource_version == "200"

    def test_update_cache_advances_resource_version_when_newer(self):
        """update_cache advances _resource_version when the incoming value is strictly newer."""
        informer = _make_informer()
        informer._resource_version = "50"
        informer.update_cache({"metadata": {"name": "foo", "resourceVersion": "99"}})
        assert informer._resource_version == "99"


class TestWorkloadInformerHandleEvent:
    """_handle_event applies watch events to the cache."""

    def test_handle_added_event_inserts_object(self):
        """ADDED event inserts the object into the cache."""
        informer = _make_informer()
        obj = {"metadata": {"name": "bar", "resourceVersion": "10"}}
        informer._handle_event({"type": "ADDED", "object": obj})
        assert informer.get("bar") == obj

    def test_handle_modified_event_replaces_object(self):
        """MODIFIED event replaces the cached object."""
        informer = _make_informer()
        informer._cache["bar"] = {"metadata": {"name": "bar", "resourceVersion": "1"}}
        updated = {"metadata": {"name": "bar", "resourceVersion": "2"}}
        informer._handle_event({"type": "MODIFIED", "object": updated})
        assert informer.get("bar") == updated

    def test_handle_deleted_event_removes_object(self):
        """DELETED event removes the object from the cache."""
        informer = _make_informer()
        informer._cache["bar"] = {"metadata": {"name": "bar"}}
        informer._handle_event({"type": "DELETED", "object": {"metadata": {"name": "bar"}}})
        assert informer.get("bar") is None

    def test_handle_event_ignores_none_object(self):
        """Events with a None object are silently ignored."""
        informer = _make_informer()
        informer._handle_event({"type": "ADDED", "object": None})
        assert informer._cache == {}

    def test_handle_event_ignores_object_without_name(self):
        """Events whose object has no metadata.name are silently ignored."""
        informer = _make_informer()
        informer._handle_event({"type": "ADDED", "object": {"metadata": {}}})
        assert informer._cache == {}

    def test_handle_event_converts_non_dict_object(self):
        """Non-dict objects are converted via to_dict() before caching."""
        informer = _make_informer()
        sdk_obj = MagicMock()
        sdk_obj.to_dict.return_value = {"metadata": {"name": "sdk-obj", "resourceVersion": "3"}}
        informer._handle_event({"type": "ADDED", "object": sdk_obj})
        assert informer.get("sdk-obj") is not None

    def test_handle_event_updates_resource_version(self):
        """_handle_event advances _resource_version from the object metadata."""
        informer = _make_informer()
        informer._handle_event({
            "type": "ADDED",
            "object": {"metadata": {"name": "foo", "resourceVersion": "77"}},
        })
        assert informer._resource_version == "77"

    def test_handle_event_does_not_downgrade_resource_version(self):
        """_handle_event never rolls back _resource_version to an older value."""
        informer = _make_informer()
        informer._resource_version = "200"
        informer._handle_event({
            "type": "MODIFIED",
            "object": {"metadata": {"name": "foo", "resourceVersion": "50"}},
        })
        assert informer._resource_version == "200"


class TestWorkloadInformerStartStop:
    """start/stop thread lifecycle."""

    def test_start_launches_daemon_thread(self):
        """start() spawns a daemon thread that is alive."""
        list_fn = MagicMock(return_value={"items": [], "metadata": {}})
        informer = WorkloadInformer(list_fn=list_fn, enable_watch=False,
                                    resync_period_seconds=9999)
        informer.start()
        assert informer._thread is not None
        assert informer._thread.is_alive()
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
