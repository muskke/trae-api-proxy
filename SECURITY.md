# Security

## Local secrets

- Keep `.env` local. It is ignored by Git and Docker build context.
- Prefer configuring both `AUTH_TOKEN` and `TRAE_IDE_TOKEN`: clients receive only the local proxy token while the upstream token stays inside the proxy.
- Bind to `127.0.0.1` when remote access is unnecessary. If binding to `0.0.0.0`, use a strong `AUTH_TOKEN` and an appropriate firewall/reverse proxy.
- Request bodies and authorization tokens are not written to normal request logs.

## Reporting

Please avoid including live account tokens, `.env` files, or captured authorization headers in public issues.
