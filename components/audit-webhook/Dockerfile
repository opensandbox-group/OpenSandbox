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

# Stage 1: build the React frontend into /static (multi-page: index,
# details, login - see frontend/vite.config.ts).
FROM node:22-slim AS frontend-build
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM python:3.12-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py store.py config.py k8s.py ./
COPY --from=frontend-build /static ./static

# Configuration: mount audit.toml at /app/audit.toml (or set AUDIT_CONFIG_PATH).
ENV AUDIT_CONFIG_PATH=/app/audit.toml
EXPOSE 8080

CMD ["python", "main.py"]
