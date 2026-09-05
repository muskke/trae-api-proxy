# Security

## Local secrets

- Keep `.env` local. It is ignored by Git and Docker build context.
- Managed browser-login credentials are stored at `TRAE_AUTH_FILE` (default `data/trae-auth.json`). The `data/` directory is ignored by Git and Docker build context.
- Credential directories are created with mode `0700` and credential files with mode `0600` where the operating system supports POSIX permissions.
- Access/refresh tokens are never returned by `/status` or `/auth/status` and are not written to normal request logs.
- OAuth provider error bodies are not copied into public errors/status, reducing the chance of session metadata being echoed into logs.
- Browser login uses PKCE S256 and one-time in-memory login state; the PKCE verifier is never exposed through status endpoints.
- Model availability state is account/session-scoped and kept in memory only. `/v1/models/status` exposes model IDs plus bounded upstream error codes/messages, never prompts or TRAE access/refresh credentials; learned state is cleared on logout or process restart.

## Network exposure

- Prefer `BIND=127.0.0.1` for personal use.
- `/auth/login` is intentionally limited to local/loopback browser flows. Docker localhost publishing is supported through a private bridge peer plus loopback Host check.
- The current TRAE callback is served on local `/authorize` (default `127.0.0.1:18080`); the legacy state callback remains only for compatibility.
- If binding the API to `0.0.0.0`, configure a strong `AUTH_TOKEN` and use an appropriate firewall/reverse proxy.
- Docker Compose publishes the service only on host `127.0.0.1` by default.
- `AUTH_TOKEN` protects client API/lifecycle endpoints but is separate from TRAE access/refresh credentials.

## Credential lifecycle

- Refresh-token rotations are serialized so concurrent requests do not attempt multiple rotations with the same credential.
- Refreshed credentials are persisted atomically. If persistence fails, the fresh in-memory session can continue temporarily and `/auth/status` exposes the persistence error without exposing tokens.
- A refresh token can still expire or be revoked. In that case the service reports `reauth_required` and a fresh local browser login is necessary.
- Treat backups of `data/trae-auth.json` as secrets and do not attach them to issues, logs, or bug reports.

## Reporting

Please avoid including live account tokens, `.env` files, `data/trae-auth.json`, captured authorization headers, or complete OAuth callback URLs containing `authCodeInfo` in public issues.
