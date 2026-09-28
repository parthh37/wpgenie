#!/usr/bin/env bash
# WPGenie installer for a fresh Ubuntu 22.04/24.04 or Debian 12 VPS.
#
#   curl -fsSL https://raw.githubusercontent.com/parthh37/wpgenie/main/deploy/install.sh | sudo bash
#
# Optional environment:
#   PANEL_DOMAIN=panel.example.com   serve the dashboard on this domain with TLS
#   ACME_EMAIL=you@example.com       Let's Encrypt account / expiry notices
#   WPGENIE_VERSION=v0.1.0           pin a release (default: latest, or main if none)
#
# Re-running is safe: secrets and data are preserved, binaries/images updated.
set -Eeuo pipefail

REPO="${WPGENIE_REPO:-parthh37/wpgenie}"
VERSION="${WPGENIE_VERSION:-latest}"
PANEL_DOMAIN="${PANEL_DOMAIN:-}"
ACME_EMAIL="${ACME_EMAIL:-}"

ETC=/etc/wpgenie
DATA=/var/lib/wpgenie
LOGS=/var/log/wpgenie
SHARE=/opt/wpgenie

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
trap 'die "installation failed at line $LINENO"' ERR

preflight() {
  [[ $EUID -eq 0 ]] || die "run as root (sudo bash)"
  [[ -r /etc/os-release ]] || die "unsupported OS"
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID}" in ubuntu | debian) ;; *) die "supported: Ubuntu 22.04+/Debian 12+ (found ${ID})" ;; esac
  case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "unsupported CPU architecture $(uname -m)" ;;
  esac
  local mem_mb
  mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
  ((mem_mb >= 900)) || warn "only ${mem_mb}MB RAM; 1GB+ recommended (2GB+ for several sites)"
  if [[ ! -f $ETC/config.json ]] && command -v ss >/dev/null && ss -ltnH '( sport = :80 or sport = :443 )' | grep -q .; then
    die "ports 80/443 are already in use (another web server?). Stop it first."
  fi
}

install_packages() {
  log "Installing base packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq ca-certificates curl openssl tar >/dev/null
  if ! command -v docker >/dev/null; then
    log "Installing Docker Engine (get.docker.com)"
    curl -fsSL https://get.docker.com | sh
  fi
  systemctl enable --now docker >/dev/null
  docker compose version >/dev/null 2>&1 || die "docker compose plugin missing"
}

resolve_version() {
  if [[ $VERSION == latest ]]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
      sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1 || true)
    [[ -n $VERSION ]] || VERSION=main
  fi
  log "Installing WPGenie ${VERSION}"
}

fetch_sources() {
  local ref_path="refs/tags/${VERSION}"
  [[ $VERSION == main ]] && ref_path="refs/heads/main"
  rm -rf "${SHARE}.new" && mkdir -p "${SHARE}.new"
  curl -fsSL "https://codeload.github.com/${REPO}/tar.gz/${ref_path}" |
    tar -xz -C "${SHARE}.new" --strip-components=1
  rm -rf "${SHARE}.old"
  [[ -d $SHARE ]] && mv "$SHARE" "${SHARE}.old"
  mv "${SHARE}.new" "$SHARE"
}

install_binary() {
  local tmp
  tmp=$(mktemp -d)
  if [[ $VERSION != main ]] &&
    curl -fsSL -o "${tmp}/wpgenie.tar.gz" "https://github.com/${REPO}/releases/download/${VERSION}/wpgenie_${VERSION}_linux_${ARCH}.tar.gz" &&
    curl -fsSL -o "${tmp}/checksums.txt" "https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"; then
    log "Verifying release checksum"
    (cd "$tmp" && grep "wpgenie_${VERSION}_linux_${ARCH}.tar.gz" checksums.txt | sed "s#wpgenie_${VERSION}_linux_${ARCH}.tar.gz#wpgenie.tar.gz#" | sha256sum -c --quiet)
    tar -xzf "${tmp}/wpgenie.tar.gz" -C "$tmp" wpgenie
  else
    log "No prebuilt release; compiling from source in a Go container"
    docker run --rm -e CGO_ENABLED=0 -v "${SHARE}:/src" -v "${tmp}:/out" -w /src golang:1.26-alpine \
      go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wpgenie ./cmd/wpgenie
  fi
  install -m 0755 "${tmp}/wpgenie" /usr/local/bin/wpgenie
  rm -rf "$tmp"
}

write_config() {
  install -d -m 0700 "$ETC"
  install -d -m 0755 "${ETC}/caddy" "$DATA" "${DATA}/sites" "${DATA}/caddy" "$LOGS"
  install -d -m 0700 "${DATA}/mariadb"
  if [[ -f ${ETC}/config.json ]]; then
    log "Keeping existing configuration and secrets"
    return
  fi
  log "Generating secrets"
  local db_root api_token shield_secret
  db_root=$(openssl rand -hex 24)
  api_token=$(openssl rand -hex 32)
  shield_secret=$(openssl rand -hex 32)
  umask 077
  cat >"${ETC}/infra.env" <<EOF
MARIADB_ROOT_PASSWORD=${db_root}
EOF
  cat >"${ETC}/config.json" <<EOF
{
  "panel_domain": "${PANEL_DOMAIN}",
  "acme_email": "${ACME_EMAIL}",
  "mariadb_dsn": "root:${db_root}@tcp(127.0.0.1:3306)/",
  "api_token": "${api_token}",
  "shield_secret": "${shield_secret}"
}
EOF
  umask 022
  # Caddy needs a valid config to boot; wpgenie replaces it on first sync.
  [[ -f ${ETC}/caddy/Caddyfile ]] || printf '{\n\tadmin 127.0.0.1:2019\n}\n' >"${ETC}/caddy/Caddyfile"
}

start_stack() {
  log "Building the hardened PHP image (a few minutes on first install)"
  docker build -q -t wpgenie/php:8.3 --build-arg PHP_VERSION=8.3 "${SHARE}/images/php" >/dev/null

  log "Starting Caddy, MariaDB and Valkey"
  docker compose -f "${SHARE}/deploy/docker-compose.yml" --env-file "${ETC}/infra.env" up -d --quiet-pull

  log "Waiting for MariaDB"
  for _ in $(seq 1 60); do
    [[ $(docker inspect -f '{{.State.Health.Status}}' wpgenie-mariadb 2>/dev/null) == healthy ]] && break
    sleep 2
  done
  [[ $(docker inspect -f '{{.State.Health.Status}}' wpgenie-mariadb) == healthy ]] || die "MariaDB did not become healthy"

  install -m 0644 "${SHARE}/deploy/wpgenie.service" /etc/systemd/system/wpgenie.service
  systemctl daemon-reload
  systemctl enable wpgenie >/dev/null 2>&1
  systemctl restart wpgenie
}

open_firewall() {
  if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
    log "Opening 80/tcp, 443/tcp and 443/udp (HTTP/3) in ufw"
    ufw allow 80/tcp >/dev/null
    ufw allow 443/tcp >/dev/null
    ufw allow 443/udp >/dev/null
  fi
}

summary() {
  local token
  token=$(sed -n 's/.*"api_token": *"\([^"]*\)".*/\1/p' "${ETC}/config.json")
  printf '\n\033[1;32mWPGenie is installed.\033[0m\n\n'
  if [[ -n $PANEL_DOMAIN ]]; then
    printf '  Dashboard:  https://%s  (point its DNS A record here)\n' "$PANEL_DOMAIN"
  else
    printf '  Dashboard:  ssh -L 8088:127.0.0.1:8088 root@<this-server>\n'
    printf '              then open http://localhost:8088\n'
  fi
  printf '  API token:  %s\n' "$token"
  printf '  CLI:        wpgenie site create example.com you@example.com\n'
  printf '  Logs:       journalctl -u wpgenie -f\n\n'
}

main() {
  preflight
  install_packages
  resolve_version
  fetch_sources
  install_binary
  write_config
  start_stack
  open_firewall
  summary
}

main "$@"
