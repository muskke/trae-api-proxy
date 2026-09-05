# ⚡ Trae API Proxy

![Trae API Proxy Banner](assets/banner.png)

一个轻量、单账号、零第三方 Go 依赖的 TRAE → OpenAI Chat Completions 兼容代理。

v0.4.0 在 v0.3 的 SOLO / legacy 协议兼容层之上补齐了 **TRAE 登录生命周期**：首次在浏览器完成一次 TRAE 登录后，代理保存 refresh token，自动换取 access token、提前刷新、轮换 refresh token，并在真实上游返回 401/403 时执行一次强制刷新与原请求重试。日常使用不再需要手工维护 `TRAE_IDE_TOKEN`。

## ✨ v0.4.0 重点

- **浏览器首次登录**：访问 `http://127.0.0.1:8000/auth/login`，登录成功后凭证自动写入本地。
- **自动续期**：默认在 access token 到期前 24 小时刷新，每 15 分钟检查一次。
- **refresh token 轮换**：ExchangeToken 返回新 refresh token 时与 access token 一起原子落盘。
- **401/403 自愈**：OAuth 会话请求上游遇到鉴权失败时，强制刷新一次并仅重试原请求一次。
- **并发刷新去重**：多个请求同时发现 Token 需要刷新时，只允许一个 refresh 流程真正执行。
- **安全凭证文件**：默认 `./data/trae-auth.json`，目录 `0700`、文件 `0600`，不写入 Git/Docker build context。
- **兼容旧配置**：`TRAE_IDE_TOKEN` 仍可作为静态模式或 `auto` 模式 fallback。
- **Docker 持久化**：Compose 使用命名卷保存 `/app/data`，重建容器不会丢登录状态。
- **SOLO + legacy**：继续支持 `/api/agent/v3/llm_utils_chat` / `solo_work_lite`，并在 `auto` 模式下对 404/405 回退旧接口。
- **OpenAI 兼容**：模型列表、流式/非流式 Chat Completions、reasoning、usage、tools / tool_choice / tool_calls。

> 项目仍保持“轻量单账号代理”的定位；多账号池、账号运营后台、签到/额度管理不在当前范围内。

## 🏗️ 调用链

```text
首次登录：
Browser → :8000/auth/login → TRAE 登录 → :18080/auth/callback/{state}
                                  ↓
                           refresh token
                                  ↓
                           ExchangeToken
                                  ↓
                    access + refresh + expiry
                                  ↓
                       data/trae-auth.json

日常请求：
OpenAI Client → trae-api-proxy → AuthManager → TRAE SOLO
                                  │
                                  ├─ 到期前：自动 refresh
                                  └─ 401/403：refresh + retry once
```

## 🚀 快速开始：推荐的无感登录模式

### 环境

- 最低：Go 1.23+
- CI 同时覆盖 Go 1.23 与当前 Go 1.27 工具链

### 1. 配置

```bash
cp .env.example .env
```

本地使用最少只需要改：

```dotenv
BIND=127.0.0.1
PORT=8000
AUTH_TOKEN=my-local-proxy-key

TRAE_AUTH_MODE=auto
TRAE_AUTH_FILE=./data/trae-auth.json
TRAE_IDE_TOKEN=
TRAE_OAUTH_CALLBACK_PORT=18080
```

`AUTH_TOKEN` 是给 OpenAI 客户端使用的本地代理 key，与 TRAE 会话完全分离。

### 2. 启动

```bash
go run ./cmd/trae-api
```

或：

```bash
make check
./bin/trae-api-proxy
```

### 3. 首次登录 TRAE

在**运行代理的同一台机器**打开：

```text
http://127.0.0.1:8000/auth/login
```

浏览器会跳转到 TRAE 登录页面。主 API 仍在 `8000`；代理会另外在本机 `127.0.0.1:18080` 启动一个仅用于 OAuth callback 的 listener。正常完成登录后，TRAE 回调该 listener，代理自动：

1. 接收一次性登录回调；
2. 取得 refresh token；
3. ExchangeToken 获取 access token；
4. 获取账号 UID；
5. 保存 `data/trae-auth.json`；
6. 开启自动刷新。

无需把 access token / refresh token 复制到 `.env`。

### 4. 确认登录状态

```bash
curl http://127.0.0.1:8000/auth/status \
  -H 'Authorization: Bearer my-local-proxy-key'
```

返回只包含安全状态，例如：

```json
{
  "mode": "auto",
  "source": "oauth",
  "authenticated": true,
  "refreshable": true,
  "auto_refresh": true,
  "needs_refresh": false,
  "uid": "...",
  "expires_at": 1788600000,
  "refresh_expires_at": 1800000000
}
```

接口和日志不会返回真实 access token / refresh token。

## 🔄 自动续期逻辑

默认策略：

```text
access token 剩余 > 24h
    → 直接使用

access token 剩余 <= 24h
    → ExchangeToken
    → 保存新的 access token
    → 如果返回新 refresh token，同时轮换保存

真实上游请求返回 401/403
    → 强制 ExchangeToken 一次
    → 清模型缓存
    → 原请求重试一次

refresh token 已过期 / 被撤销
    → reauth_required
    → 再访问 /auth/login 完成一次网页登录
```

并发请求不会重复消费同一个 refresh token；刷新操作有独立互斥锁并会在拿锁后重新检查凭证状态。

手工触发一次刷新：

```bash
curl -X POST http://127.0.0.1:8000/auth/refresh \
  -H 'Authorization: Bearer my-local-proxy-key'
```

退出并删除本地 OAuth 凭证：

```bash
curl -X POST http://127.0.0.1:8000/auth/logout \
  -H 'Authorization: Bearer my-local-proxy-key'
```

## 🔐 认证模式

### `TRAE_AUTH_MODE=auto`（推荐）

优先级：

```text
有效的 data/trae-auth.json
       ↓ 无
TRAE_IDE_TOKEN
       ↓ 无
请求中的 Bearer / X-API-Key
```

如果设置了 `AUTH_TOKEN`，客户端提供的 Bearer 是本地代理 key，不会被误当成 TRAE Token；因此推荐同时使用浏览器 OAuth 或静态 `TRAE_IDE_TOKEN`。

### `TRAE_AUTH_MODE=oauth`

只接受浏览器登录产生的托管凭证。没有有效会话时返回 `reauth_required`。

### `TRAE_AUTH_MODE=token`

完全兼容旧版：

```dotenv
TRAE_AUTH_MODE=token
TRAE_IDE_TOKEN=your-trae-access-token
TRAE_UID=
TRAE_DEVICE_ID=
TRAE_MACHINE_ID=
```

不做 OAuth 自动续期。

### `TRAE_AUTH_MODE=passthrough`

客户端请求中的 Bearer Token / `X-API-Key` 直接作为 TRAE Token，适合明确需要请求级凭证的场景。

## 🔗 API

| Method | Path | 用途 |
|---|---|---|
| `GET` | `/healthz` | 健康检查，无需鉴权 |
| `GET` | `/status` | 服务 + 安全认证状态 |
| `GET` | `/auth/status` | OAuth 生命周期状态 |
| `GET` | `/auth/login` | 从 localhost 发起 TRAE 浏览器登录 |
| `GET` | `/auth/callback/{state}` | 一次性 TRAE 登录回调 |
| `POST` | `/auth/refresh` | 手工强制刷新一次 |
| `POST` | `/auth/logout` | 删除本地托管凭证 |
| `GET` | `/v1/models` | OpenAI-compatible 模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI-compatible Chat Completions |

### 模型列表

```bash
curl http://127.0.0.1:8000/v1/models \
  -H 'Authorization: Bearer my-local-proxy-key'
```

### 非流式对话

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H 'Authorization: Bearer my-local-proxy-key' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": false
  }'
```

### 流式对话 + usage

```bash
curl -N http://127.0.0.1:8000/v1/chat/completions \
  -H 'Authorization: Bearer my-local-proxy-key' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role": "user", "content": "写一个 Go hello world"}],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

## 🧰 Tool Calling

OpenAI 风格的 `tools` / `tool_choice` 会转换为 SOLO 所需结构；上游增量 function call 会重新拼接为 OpenAI `tool_calls`。支持：

- `tool_choice: "auto"`
- `"required"`
- `"none"`
- 指定 function 对象
- assistant 历史中的 `tool_calls`
- `role=tool` 的工具结果回传

## 🗺️ 模型映射

```dotenv
TRAE_MODEL_ALIASES=sonnet=glm-5.2;fast=glm-5-turbo
```

映射 key 不区分大小写；`model__suffix` 形式也会先归一化。

## ⚙️ 主要配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `BIND` | `0.0.0.0` | HTTP 监听地址；`.env.example` 为本地安全起见预设 `127.0.0.1` |
| `PORT` | `8000` | HTTP 端口 |
| `AUTH_TOKEN` | 空 | 客户端访问代理的本地 key |
| `TRAE_AUTH_MODE` | `auto` | `auto` / `oauth` / `token` / `passthrough` |
| `TRAE_AUTH_FILE` | `./data/trae-auth.json` | 托管 OAuth 凭证 |
| `TRAE_IDE_TOKEN` | 空 | 兼容静态 Token / auto fallback |
| `TRAE_OAUTH_CALLBACK_PORT` | `18080` | 本机 OAuth callback listener 端口 |
| `TRAE_OAUTH_CALLBACK_BASE_URL` | 空（自动派生 `http://127.0.0.1:18080`） | 高级自定义回调基址 |
| `TRAE_OAUTH_CALLBACK_BIND` | 空 | callback listener 绑定地址；Compose 内部覆盖为 `0.0.0.0` |
| `TRAE_OAUTH_REFRESH_SKEW` | `24h` | 提前多久刷新 access token |
| `TRAE_OAUTH_REFRESH_INTERVAL` | `15m` | 后台检查间隔 |
| `TRAE_OAUTH_LOGIN_TTL` | `10m` | 一次性登录 state 有效期 |
| `TRAE_UPSTREAM_MODE` | `auto` | `auto` / `solo` / `legacy` |
| `TRAE_API_BASE_URL` | `https://trae-api-cn.mchost.guru` | TRAE Agent host |
| `TRAE_DEFAULT_MODEL` | `glm-5.2` | 默认模型 |
| `TRAE_MODEL_ALIASES` | 空 | `alias=model` 列表 |
| `TRAE_MODEL_CACHE_TTL` | `1h` | 模型列表缓存 |
| `TRAE_REQUEST_TIMEOUT` | `120s` | 短 JSON 请求总超时 |
| `TRAE_RESPONSE_HEADER_TIMEOUT` | `120s` | 上游首包/响应头超时 |
| `MAX_BODY_BYTES` | `8388608` | 请求体上限（8 MiB） |
| `SHUTDOWN_TIMEOUT` | `10s` | 优雅退出等待时间 |
| `CORS_ALLOW_ORIGIN` | 空 | 留空关闭 CORS |

OAuth host、Client ID、ExchangeToken/UserInfo 路径等兼容参数也都可在 `.env.example` 中覆盖，方便上游协议再次变化时无需改代码。

## 🐳 Docker

```bash
cp .env.example .env
# 编辑 AUTH_TOKEN 等配置

docker compose up -d --build
```

Compose 默认：

- 主 API 只映射宿主机 `127.0.0.1:${PORT}`；
- OAuth callback 只映射宿主机 `127.0.0.1:${TRAE_OAUTH_CALLBACK_PORT:-18080}`；
- 容器内主 API 和 callback listener 分别监听容器端口；callback URL 对浏览器仍是宿主机 `127.0.0.1:18080`；
- 用命名卷 `trae-api-data` 持久化 `/app/data`；
- 重建/升级容器后仍保留 OAuth 登录状态。

第一次仍在宿主机浏览器打开：

```text
http://127.0.0.1:8000/auth/login
```

停止服务不会删除凭证：

```bash
docker compose down
```

如果明确要连凭证卷一起删除：

```bash
docker compose down -v
```

## 🧪 开发与验证

```bash
make test
make vet
make build
# 或
make check
```

离线测试全部使用 `httptest`，不需要真实 TRAE 账号，覆盖：

- OAuth callback → ExchangeToken → UserInfo → `0600` 持久化
- access/refresh token 轮换
- 过期前主动刷新
- 并发刷新去重
- 刷新失败时有效旧 Token 的容错
- refresh token 失效后的 `reauth_required`
- 上游 401 → 强制刷新 → 原 chat 请求只重试一次
- OAuth UID / machine ID / device ID 注入上游请求
- `/auth/status` 不泄漏 access / refresh token
- SOLO payload、tools / tool_choice、SSE、usage
- SOLO → legacy 自动回退
- 动态模型列表与缓存
- 本地 `AUTH_TOKEN` 与上游凭证隔离
- dotenv 配置解析

## ⬆️ 从 v0.3.0 升级

1. 保留原 `.env`，新增 `TRAE_AUTH_MODE=auto` 与 `TRAE_AUTH_FILE=./data/trae-auth.json`。
2. 如果想继续静态 Token，什么都不必迁移：原 `TRAE_IDE_TOKEN` 会作为 `auto` fallback；也可以显式设 `TRAE_AUTH_MODE=token`。
3. 要启用无感续期，确保本机 `18080` 未被占用（或调整 `TRAE_OAUTH_CALLBACK_PORT`），启动后访问 `/auth/login` 完成一次网页登录。
4. 登录成功后可删除 `.env` 里的 `TRAE_IDE_TOKEN`；托管凭证会保存在 `data/trae-auth.json`。
5. Docker 用户升级 Compose 后会使用持久化卷；首次升级需要重新网页登录一次以把凭证写入卷。

## 🔎 公开实现参考

认证和上游兼容实现对照了以下公开项目，本仓库仍保持自己的轻量单账号定位：

- [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) — browser login、refreshToken、ExchangeToken、凭证轮换与 SOLO upstream。
- [connectedGraph/trae2api-web](https://github.com/connectedGraph/trae2api-web) — Web 登录闭环、账号热加载、动态模型和工程化参考。
- [wanlibeiqiu/local-proxy](https://github.com/wanlibeiqiu/local-proxy) — SSE、模型映射和本地 key 隔离的代理设计参考。

## 📂 目录

```text
.
├── cmd/trae-api/              # 服务入口
├── internal/auth/             # 浏览器登录 / 凭证 / 自动刷新
├── internal/config/           # 配置与校验
├── internal/handler/          # HTTP API / 鉴权 / 错误映射
├── internal/middleware/       # request ID / CORS / 日志 / recovery
├── internal/service/trae/     # SOLO + legacy upstream / SSE / payload
├── pkg/utils/                 # dotenv 工具
├── data/                      # 本地 OAuth 凭证（Git ignored）
├── .github/workflows/ci.yml
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── CHANGELOG.md
```

## ⚠️ 兼容边界

- 当前专注 Chat Completions，不实现 embeddings、images、audio、OpenAI Responses API 或多账号池。
- access token 可以自动刷新，但 refresh token 本身仍可能过期、被撤销或因账号状态变化失效；此时需要重新访问 `/auth/login`。
- OAuth/Agent 上游并非公开稳定 API。所有 host、Client ID、路径和客户端指纹均做成可配置项，以降低后续协议漂移的维护成本。

## 📄 License

MIT，见 [LICENSE](LICENSE)。
