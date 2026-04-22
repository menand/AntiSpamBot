FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 bot && \
    mkdir -p /data && chown bot:bot /data
COPY --from=build /out/bot /usr/local/bin/bot
USER bot
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/bot"]
