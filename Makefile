VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux test test-integration lint php-image clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/wpgenie ./cmd/wpgenie

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/wpgenie-linux-amd64 ./cmd/wpgenie
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/wpgenie-linux-arm64 ./cmd/wpgenie

test:
	go test -race ./...

# Also validates the generated Caddyfile with a real Caddy (needs Docker).
test-integration:
	WPGENIE_TEST_DOCKER=1 go test -race -count=1 ./...

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	go vet ./...

php-image:
	docker build -t wpgenie/php:8.3 images/php

clean:
	rm -rf bin
