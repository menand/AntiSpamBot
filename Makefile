.PHONY: build run test vet tidy docker-up docker-down docker-logs clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/bot ./cmd/bot

run:
	go run ./cmd/bot

test:
	go test -race ./...

vet:
	go vet ./...

tidy:
	go mod tidy

docker-up:
	VERSION=$(VERSION) docker compose up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f

clean:
	rm -rf bin/
