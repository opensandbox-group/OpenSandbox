#!/bin/bash

# Copyright 2025 The OpenSandbox Authors
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

set -euo pipefail

# There are a lot of interesting flags we could use here: https://github.com/microsoft/playwright/blob/20023ab33a1dc04db2d5a3f753760eef33339e73/packages/playwright-core/src/server/chromium/chromiumSwitches.ts#L47
flags=()

flags+=(--no-sandbox) # We can't use sandbox in a container

flags+=(--disable-gpu)           # We don't (normally) have a GPU
flags+=(--disable-dev-shm-usage) # We don't (normally) have a shared memory filesystem

flags+=(--no-default-browser-check) # Avoids hanging with a "set chrome as default browser" dialog
flags+=(--no-first-run)             # Avoids hanging with a "set chrome as default browser" dialog

flags+=(--start-maximized) # We're the only thing running, use the whole screen

flags+=(--disable-field-trial-config) # Keeps things consistent and a little faster (?)

flags+=(--remote-debugging-port=9222)     # Enable remote debugging
flags+=(--user-data-dir=/tmp/chrome-data) # DevTools remote debugging requires a non-default data directory. Specify this using --user-data-dir.

# Launch Chrome
exec chromium "${flags[@]}" "https://www.google.com"
