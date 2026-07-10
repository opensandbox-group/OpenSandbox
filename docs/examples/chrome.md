---
title: Chrome
description: Run Chrome Browser with OpenSandbox runtime for remote debugging via DevTools.
---

# Chrome Browser in OpenSandbox

This example runs Chrome Browser with OpenSandbox runtime.

The image starts a VNC server (`Xtigervnc :1`) and launches Chromium with remote debugging enabled on port `9222`.

## Getting Chrome image

You can build the image from source or pull it from Docker Hub.

### Build from source

```shell
cd examples/chrome
docker build -t opensandbox/chrome .
```

### Pull an existing image

```shell
docker pull opensandbox/chrome:latest

# use acr from china
# docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/chrome:latest
```

## Start OpenSandbox server

Start the OpenSandbox server and tail stdout from the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and access a Chrome sandbox

Build/pull the image above, then create a sandbox with image `opensandbox/chrome:latest` and an entrypoint that keeps it
alive (e.g., `["/bin/sh", "-c", "sleep infinity"]`), or reuse `tail -f /dev/null`. Make sure the runtime exposes ports
`5901` and `9222` for VNC/DevTools.

```shell
uv pip install opensandbox
uv run python examples/chrome/main.py
```

Then fetch endpoints for 5901/9222 to connect with a VNC client or DevTools, like:

```text
execd daemon running with endpoint='127.0.0.1:48379/proxy/44772'
VNC running with endpoint='127.0.0.1:48379/proxy/5901'
DevTools running with endpoint='127.0.0.1:48379/proxy/9222'/json
```

```text
[ {
   "description": "",
   "devtoolsFrontendUrl": "https://chrome-devtools-frontend.appspot.com/serve_rev/@.../inspector.html?ws=127.0.0.1:52302/devtools/page/...",
   "faviconUrl": "https://www.gstatic.com/images/branding/searchlogo/ico/favicon.ico",
   "id": "2215AF60AC345E4BA6D822389CFC743B",
   "title": "Google",
   "type": "page",
   "url": "https://www.google.com.hk/",
   "webSocketDebuggerUrl": "ws://127.0.0.1:52302/devtools/page/2215AF60AC345E4BA6D822389CFC743B"
} ]
```

Or you can use it by MCP client, more information please refer
to: [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp).

## References

- [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/chrome)
