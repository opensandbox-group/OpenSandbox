# Console sandbox shell

## Goal

从开发者控制台进入正在运行的沙箱，使用真实终端查看和操作沙箱内的文件。lifecycle server 与 execd 的代码不改。控制台 BFF 桥接已有的 execd PTY，Web 页面提供终端。

## Users and entry points

租户和 Admin 都能使用。三处入口都链接到同一个页面 `/sandboxes/:id/shell`：

- 租户沙箱列表的操作列
- Admin 全局沙箱列表的操作列，链接带上已有的 `?tenant=`
- 沙箱详情页的操作区

只有状态为 `Running` 的沙箱可以打开。这是完整 shell，可以改文件、执行命令。

## Connection

浏览器里的 xterm 只连接 BFF，使用现有 httpOnly 会话 cookie。execd 访问令牌和租户 API Key 不出 BFF。

```text
xterm
  → BFF WebSocket
      → GET /sandboxes/{id}/endpoints/44772    lifecycle，只要地址和请求头
      → POST {endpoint}/pty                     execd，创建会话
      → GET  {endpoint}/pty/{sessionId}/ws      execd，双向转发帧
```

租户会话使用该租户的 Key。Admin 会话使用 `?tenant=` 对应的租户 Key。

一次页面打开对应一个 PTY。浏览器断开、离开页面或刷新时，BFF 关闭到 execd 的 WebSocket，并 `DELETE /pty/{sessionId}`。刷新是一次新 shell。

若 lifecycle 把 endpoint 改写成 server proxy（路径同时包含 `/sandboxes/` 和 `/proxy/`），BFF 在发往该地址的请求上附加 `OPEN-SANDBOX-API-KEY`。直连 execd 时不附带租户 Key。endpoint 响应里的 headers（包括 `X-EXECD-ACCESS-TOKEN` 和 ingress 路由头）原样转发。

没有 scheme 的 endpoint 默认跟随 `LIFECYCLE_API_BASE` 的 scheme。`BFF_EXECD_PROTOCOL=http|https` 可以覆盖。endpoint 字符串自身带 `http://` 或 `https://` 时，以该 scheme 为准。

## Browser protocol

BFF 透明转发 execd 已有的 WebSocket 帧，不另做一套协议。

- 浏览器 → execd：二进制帧 `0x00` + stdin；文本帧 `{"type":"resize","cols":N,"rows":N}`
- execd → 浏览器：`0x01` stdout、`0x02` stderr、`0x03` + 8 字节 offset + replay；文本帧 `connected`、`exit`、`error`

连接建立前的失败（未登录、缺少 tenant、endpoint 或创建 PTY 失败）由 BFF 发送一条 `{"type":"error","code":"...","error":"..."}` 后关闭。

## Out of scope

- 不修改 `server/`、`components/execd/`、公开 spec、SDK、CLI
- 不把 `/pty` 补进 `execd-api.yaml`
- 不保留可重连的 PTY，不提供只读 viewer
- 不在 BFF 里记录命令内容

## Verification

- BFF 单测覆盖 endpoint URL、直连时不泄漏 API Key、server proxy 时附带 API Key、会话结束后删除 PTY、未登录拒绝
- `console/web` 的 `tsc` 与生产构建通过
