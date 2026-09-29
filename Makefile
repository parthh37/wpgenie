VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux test test-integration test-e2e lint php-image caddy-image release-key clean

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

# Phase 2 end to end (backups, restores, staging, pushes, domains, PHP 8.4,
# SFTP, Adminer) against real WordPress, MariaDB and restic. Runs the tests in
# a container on a Docker network shared with the database; the temporary
# directory is mounted at the same path so sibling containers' bind mounts
# resolve. Needs the PHP images (make php-image, and PHP_VERSION=8.4).
# E2E_TMP must be root-owned all the way up (not under /tmp): sshd refuses to
# chroot below a group/world-writable directory, as it should.
E2E_TMP ?= /var/lib/wpgenie-e2e
test-e2e:
	docker network inspect wpgenie-e2e >/dev/null 2>&1 || docker network create wpgenie-e2e
	test -d $(E2E_TMP) || sudo install -d -m 0755 -o root -g root $(E2E_TMP)
	docker run --rm --network wpgenie-e2e -v /var/run/docker.sock:/var/run/docker.sock -v "$(CURDIR)":/src -w /src \
	  -v $(E2E_TMP):$(E2E_TMP) -e TMPDIR=$(E2E_TMP) -e WPGENIE_TEST_DOCKER=1 -e WPGENIE_TEST_E2E=1 -e E2E_NET=wpgenie-e2e \
	  golang:1.26-alpine sh -c 'apk add -q docker-cli >/dev/null && go test -count=1 -timeout 30m \
	    -run "EndToEnd|SFTPServer|ResticRoundTrip|OpenSSL" ./internal/...'

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	go vet ./...

php-image:
	docker build -t wpgenie/php:8.3 images/php
	docker build -t wpgenie/php:8.4 --build-arg PHP_VERSION=8.4 images/php

# Caddy with the Coraza WAF module (request-body inspection).
caddy-image:
	docker build -t wpgenie/caddy:2 images/caddy

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
