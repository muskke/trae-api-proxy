# Changelog

All notable changes to this project are documented here.

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
