# Changelog

All notable changes to this project are documented here.

## [0.4.0] - 2026-09-05

### Added

- Managed TRAE browser-login flow through `/auth/login` and one-time `/auth/callback/{state}` callbacks, with a dedicated localhost callback listener (default port `18080`).
- Persistent OAuth credential storage with access token, rotating refresh token, expiry metadata, UID, machine ID, and device ID.
- Proactive access-token refresh before expiry and configurable background refresh checks.
- Single-flight refresh locking so concurrent API requests do not rotate the same refresh token multiple times.
- One-time automatic recovery for upstream `401` / `403`: force refresh, invalidate model cache, and retry the original request once.
- `/auth/status`, `/auth/refresh`, and `/auth/logout` lifecycle endpoints without exposing raw credentials.
- Re-authentication status when the refresh session expires or can no longer be renewed.
- Offline tests for browser callback exchange, credential persistence, refresh rotation/concurrency/failure behavior, 401 recovery, and OAuth device identity propagation.

### Changed

- `TRAE_AUTH_MODE=auto` is now the default and prefers managed OAuth credentials, then the legacy static `TRAE_IDE_TOKEN`, then request passthrough where applicable.
- `AUTH_TOKEN` no longer requires a static `TRAE_IDE_TOKEN`; it can be used with managed OAuth authentication.
- OAuth credentials are atomically persisted with private filesystem permissions and excluded from Git/Docker build contexts.
- Provider error response bodies are not copied into public auth status/errors.
- Local browser login accepts loopback traffic and Docker's private bridge-to-loopback-host pattern while rejecting public Host-header spoofing.
- Docker Compose now persists `/app/data` in a named volume, publishes the OAuth callback port on host localhost, and grants the unprivileged app user write access to the data directory.

### Compatibility

- `TRAE_AUTH_MODE=token` preserves the v0.3 static-token workflow.
- Existing `TRAE_IDE_TOKEN`, UID, machine-ID, device-ID, model, SOLO/legacy, and OpenAI client settings remain supported.
- OAuth host, console URL, client ID, exchange/user-info paths, callback base, plugin version, refresh skew, refresh interval, and login TTL are configurable for future upstream changes.

## [0.3.0] - 2026-09-05

### Added

- Current TRAE SOLO upstream support through `llm_utils_chat` / `solo_work_lite`.
- Automatic protocol selection with legacy endpoint fallback on endpoint mismatch (`404` / `405`).
- Dynamic model discovery with an in-memory TTL cache and stale-cache fallback.
- OpenAI-compatible tool-call streaming/aggregation, `reasoning_content`, and usage chunks.
- `stream_options.include_usage` support.
- `/healthz`, `/status`, request IDs, optional CORS, response-size logging, and security headers.
- Configurable model aliases and a default-model fallback.
- Request body limits and configurable upstream/header/shutdown timeouts.
- Dockerfile, Docker Compose, Makefile, and GitHub Actions CI.
- Offline unit/integration tests for protocol transforms, SSE handling, authentication, fallback, and model caching.

### Changed

- Updated current SOLO fingerprint defaults to IDE `0.1.52` / version code `20260811`.
- Updated the default upstream host to `https://trae-api-cn.mchost.guru`.
- Non-stream requests now use the same upstream SSE path as stream requests and are aggregated locally.
- Local `AUTH_TOKEN` comparison is constant-time; the configured TRAE token is never exposed to clients.
- `.env` parsing now correctly handles quotes, inline comments, `export`, and process-environment precedence.
- Removed third-party Go dependencies; the proxy now builds with the standard library only.
- Minimum Go language version is 1.23; CI also validates against the current Go 1.27 line.

### Compatibility

- Existing legacy `/api/ide/v1/chat` and `model_list` behavior remains available with `TRAE_UPSTREAM_MODE=legacy`.
- `TRAE_UPSTREAM_MODE=auto` is the recommended migration setting.

## [0.2.0]

- Added custom client token authentication, graceful shutdown, context propagation, middleware, and non-stream aggregation.
