.PHONY: build run test vet tidy docker-up docker-down docker-logs clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# -tags stdjson — стандартный encoding/json вместо grbit/go-json: тот падает
# nil-pointer'ом в Update.Clone() на типизированном nil в интерфейсных полях
# (см. «panic in handler» в проде). Телего официально поддерживает тег.
JSON_TAGS := -tags stdjson

build:
	go build $(JSON_TAGS) -ldflags "$(LDFLAGS)" -o bin/bot ./cmd/bot

run:
	go run $(JSON_TAGS) ./cmd/bot

test:
	go test $(JSON_TAGS) -race ./...

vet:
	go vet $(JSON_TAGS) ./...

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
