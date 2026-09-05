FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trae-api-proxy ./cmd/trae-api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app -u 10001 app

COPY --from=build /out/trae-api-proxy /usr/local/bin/trae-api-proxy
RUN mkdir -p /app/data && chown -R app:app /app
USER app
WORKDIR /app

ENV BIND=0.0.0.0 \
    PORT=8000
EXPOSE 8000 18080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O - "http://127.0.0.1:${PORT:-8000}/healthz" | grep -q '^ok$' || exit 1

ENTRYPOINT ["trae-api-proxy"]
