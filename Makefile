VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux test test-integration lint php-image release-key clean

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

# Generates the Ed25519 release signing key pair (OpenSSL 3 in a container:
# macOS ships LibreSSL). Commit internal/updater/release.pub; store
# release-signing.key as the RELEASE_SIGNING_KEY secret and keep it offline.
release-key:
	@test ! -s release-signing.key || { echo "release-signing.key exists; refusing to overwrite"; exit 1; }
	docker run --rm -v "$(CURDIR):/w" -w /w -e OWNER="$$(id -u):$$(id -g)" alpine:3 sh -ec \
	  'apk add -q --no-cache openssl >/dev/null; umask 077; \
	   openssl genpkey -algorithm ed25519 -out release-signing.key; \
	   openssl pkey -in release-signing.key -pubout -out internal/updater/release.pub; \
	   chmod 644 internal/updater/release.pub; chown "$$OWNER" release-signing.key internal/updater/release.pub'
	@echo "Public key written to internal/updater/release.pub (commit it)."
	@echo "Add the contents of release-signing.key as the RELEASE_SIGNING_KEY GitHub secret, then delete it here."

clean:
	rm -rf bin
