# syntax=docker/dockerfile:1.7
# WPGenie PHP runtime: official WordPress FPM image + Redis extension,
# WP-CLI and a hardened FPM pool. Rebuilt by CI; PHP_VERSION selects the line.
ARG PHP_VERSION=8.3
FROM wordpress:php${PHP_VERSION}-fpm-alpine

ARG WP_CLI_VERSION=2.12.0
ARG WP_CLI_SHA256=ce34ddd838f7351d6759068d09793f26755463b4a4610a5a5c0a97b68220d85c

RUN set -eux; \
    apk add --no-cache --virtual .build-deps $PHPIZE_DEPS; \
    pecl install redis; \
    docker-php-ext-enable redis; \
    apk del .build-deps; \
    rm -rf /tmp/pear

ADD --checksum=sha256:${WP_CLI_SHA256} \
    https://github.com/wp-cli/wp-cli/releases/download/v${WP_CLI_VERSION}/wp-cli-${WP_CLI_VERSION}.phar \
    /usr/local/bin/wp
RUN chmod 0755 /usr/local/bin/wp

COPY php.ini  /usr/local/etc/php/conf.d/zz-wpgenie.ini
COPY pool.conf /usr/local/etc/php-fpm.d/zz-wpgenie.conf

# The container root filesystem is read-only at runtime; point every cache
# at the tmpfs.
ENV WP_CLI_CACHE_DIR=/tmp/wp-cli-cache \
    WP_CLI_PACKAGES_DIR=/tmp/wp-cli-packages \
    HOME=/tmp

USER 82:82
