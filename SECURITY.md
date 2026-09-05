# Security

## Local secrets

- Keep `.env` local. It is ignored by Git and Docker build context.
- Managed browser-login credentials are stored at `TRAE_AUTH_FILE` (default `data/trae-auth.json`). The `data/` directory is ignored by Git and Docker build context.
- Credential directories are created with mode `0700` and credential files with mode `0600` where the operating system supports POSIX permissions.
- Access/refresh tokens are never returned by `/status` or `/auth/status` and are not written to normal request logs.
- OAuth provider error bodies are not copied into public errors/status, reducing the chance of session metadata being echoed into logs.
- Browser login uses PKCE S256 and one-time in-memory login state; the PKCE verifier is never exposed through status endpoints.
- Model availability state is account/session-scoped and persisted at `TRAE_MODEL_STATUS_FILE` (default `data/model-status.json`) with private/atomic local writes. `/v1/models/status` exposes model IDs plus bounded upstream error codes/messages, never prompts or TRAE access/refresh credentials; logout clears learned state.

## Network exposure

- Prefer `BIND=127.0.0.1` for personal use.
- `/auth/login` is intentionally limited to local/loopback browser flows. Docker localhost publishing is supported through a private bridge peer plus loopback Host check.
- The current TRAE callback is served on local `/authorize` (default `127.0.0.1:18080`); the legacy state callback remains only for compatibility.
- If binding the API to `0.0.0.0`, configure a strong `AUTH_TOKEN` and use an appropriate firewall/reverse proxy.
- Docker Compose publishes the service only on host `127.0.0.1` by default.
- `AUTH_TOKEN` protects client API/lifecycle endpoints, including `/v1/chat/completions` and `/v1/responses`, but is separate from TRAE access/refresh credentials.
- `/v1/responses` is intentionally stateless: the proxy does not persist request input, tool output, reasoning text, or Response objects. Client-executed tool definitions and outputs only exist for the lifetime of the request.

## Tool execution boundary

- Function/custom/namespace and client-executed `tool_search` calls are protocol bridges only; the proxy does not execute client tools itself. Apply filesystem/shell/MCP permissions in the Codex/Agent runtime that owns those tools.
- Responses `web_search` is executed by the proxy through its configured search runtime. Model-directed `open_page` / `find_in_page` requests are restricted to public HTTP(S) destinations and reject loopback, private, link-local, multicast, and resolved private addresses. Search backend endpoints are administrator-controlled configuration.
- Other hosted Responses tools such as direct `type: mcp`, file search, code interpreter, and computer use remain rejected rather than forwarded to an unknown runtime.
- Avoid exposing the proxy to untrusted clients with a shared `AUTH_TOKEN`: tool schemas and tool outputs can contain sensitive project data even though the proxy does not persist them.

## Credential lifecycle

- Refresh-token rotations are serialized so concurrent requests do not attempt multiple rotations with the same credential.
- Refreshed credentials are persisted atomically. If persistence fails, the fresh in-memory session can continue temporarily and `/auth/status` exposes the persistence error without exposing tokens.
- A refresh token can still expire or be revoked. In that case the service reports `reauth_required` and a fresh local browser login is necessary.
- Treat backups of `data/trae-auth.json` as secrets. `data/model-status.json` does not contain tokens or prompts, but it can contain account-scoped model IDs and bounded upstream error messages, so keep the whole `data/` directory private by default.

## Reporting

Please avoid including live account tokens, `.env` files, `data/trae-auth.json`, captured authorization headers, complete OAuth callback URLs containing `authCodeInfo`, or an unreviewed copy of the local `data/` directory in public issues.
