.PHONY: build run test vet tidy docker-up docker-down docker-logs clean

build:
	go build -o bin/bot ./cmd/bot

run:
	go run ./cmd/bot

test:
	go test -race ./...

vet:
	go vet ./...

tidy:
	go mod tidy

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f

clean:
	rm -rf bin/
