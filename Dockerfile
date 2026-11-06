FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -tags stdjson -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/bot ./cmd/bot
# msgtool — редкий аналитический доступ к следу сообщений (hot-окно + архивы).
# Тот же образ, чтобы на проде не тащить отдельный бинарь: docker exec.
RUN CGO_ENABLED=0 GOOS=linux go build -tags stdjson -trimpath \
    -ldflags="-s -w" -o /out/msgtool ./cmd/msgtool

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 bot && \
    mkdir -p /data && chown bot:bot /data
COPY --from=build /out/bot /usr/local/bin/bot
COPY --from=build /out/msgtool /usr/local/bin/msgtool
COPY --chmod=755 scripts/healthcheck.sh /usr/local/bin/healthcheck
USER bot
VOLUME ["/data"]
# Бот раз в минуту освежает /data/.heartbeat (рядом с DB_PATH); свежесть файла
# — единственный честный признак жизни: зависший процесс остаётся «running»,
# а тихий чат не пишет в лог часами.
HEALTHCHECK --interval=2m --timeout=10s --start-period=2m --retries=3 \
    CMD ["/usr/local/bin/healthcheck"]
ENTRYPOINT ["/usr/local/bin/bot"]
