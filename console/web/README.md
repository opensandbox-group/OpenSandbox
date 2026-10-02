# OpenSandbox Console Web

React + Vite + Ant Design SPA，通过 BFF（`/api`，cookie 会话）访问 Lifecycle，不持有 API Key。

## 开发

终端 1 — BFF（见 [../README.md](../README.md)）：

```bash
cd ../bff && source .venv/bin/activate
uvicorn app.main:app --reload --port 8091
```

终端 2 — 前端：

```bash
npm install
npm run dev
```

浏览器打开 Vite 提示的地址（默认 `http://localhost:5173`）。`/login` 支持 **租户 API Key** 与 **Admin Token** 两个 Tab。

## 构建

```bash
npm run build
```

产物在 `dist/`，可由 Ingress 或与 BFF 同域静态托管。

## 路由与阶段

| 路径 | 一期 | 二期 | 说明 |
|------|------|------|------|
| `/` | ✓ | ✓ | 概览 KPI（租户 / Admin） |
| `/sandboxes` | ✓ | | 租户沙箱列表 |
| `/sandboxes/new` | ✓ | | 创建 |
| `/sandboxes/:id` | ✓ | | 详情、续期、endpoint、pause/resume |
| `/admin/sandboxes` | | ✓ | Admin 全局列表 |
| `/snapshots` | | ✓ | 列表 / 删除 / 从沙箱创建 |
| `/pools` | | ✓ | Admin；Pool 非租户隔离提示 |
| `/diagnostics` | | ✓ | logs / events |
| `/platform/*` | | ✓ | 健康、版本；K8s 页为 RBAC 占位 |
