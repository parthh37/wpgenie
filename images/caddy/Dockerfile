# Caddy with the Coraza WAF module (OWASP Core Rule Set embedded), for
# request-body inspection. Everything else is the official image.
ARG CADDY_VERSION=2.11.4
FROM caddy:${CADDY_VERSION}-builder-alpine AS build
ARG CORAZA_CADDY_VERSION=v2.6.1
RUN xcaddy build --with github.com/corazawaf/coraza-caddy/v2@${CORAZA_CADDY_VERSION}

FROM caddy:${CADDY_VERSION}-alpine
COPY --from=build /usr/bin/caddy /usr/bin/caddy
# COPY drops file capabilities: restore the one that lets an unprivileged
# Caddy bind :80 and :443 (the official image sets it the same way).
RUN setcap cap_net_bind_service=+ep /usr/bin/caddy && caddy list-modules | grep -q '^http.handlers.waf$'
