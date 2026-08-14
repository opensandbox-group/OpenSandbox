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

from email.utils import parsedate_to_datetime

from fastapi.testclient import TestClient

from opensandbox_server.main import app


class TestHealthCheck:

    def test_health_check(self, client: TestClient):
        response = client.get("/health")
        assert response.status_code == 200
        assert response.json() == {"status": "healthy"}
        assert parsedate_to_datetime(response.headers["date"]).tzinfo is not None

    def test_unhandled_error_has_date_header(self, auth_headers: dict):
        async def raise_unhandled_error():
            raise RuntimeError("test unhandled error")

        app.add_api_route("/_test/unhandled-error", raise_unhandled_error)
        route = app.router.routes.pop()
        app.router.routes.insert(0, route)
        error_client = TestClient(app, raise_server_exceptions=False)

        try:
            response = error_client.get(
                "/_test/unhandled-error",
                headers=auth_headers,
            )
        finally:
            error_client.close()
            app.router.routes.remove(route)

        assert response.status_code == 500
        dates = response.headers.get_list("date")
        assert len(dates) == 1
        assert parsedate_to_datetime(dates[0]).tzinfo is not None


class TestVersionInfo:

    def test_version_endpoint(self, client: TestClient):
        response = client.get("/version")
        assert response.status_code == 200
        assert isinstance(response.json()["version"], str)
        assert response.json()["version"]
