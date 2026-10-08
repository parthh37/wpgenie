# syntax=docker/dockerfile:1.7
# WPGenie PHP runtime: official WordPress FPM image + Redis extension,
# WP-CLI and a hardened FPM pool. Rebuilt by CI; PHP_VERSION selects the line
# (8.2, 8.3, 8.4: tagged wpgenie/php:<version>; the daemon builds the ones
# other than the default when a site first switches to them).
ARG PHP_VERSION=8.3
FROM wordpress:php${PHP_VERSION}-fpm-alpine

ARG WP_CLI_VERSION=2.12.0
ARG WP_CLI_SHA256=ce34ddd838f7351d6759068d09793f26755463b4a4610a5a5c0a97b68220d85c
ARG REDIS_CACHE_VERSION=3.0.0
ARG REDIS_CACHE_SHA256=7a03342d3defbe94cac8d0470b42dc1a1c34331b19c47205df20ceccd3c8f54a
# Brotli (bundled library): the page cache stores .br copies of its pages.
ARG BROTLI_VERSION=0.21.0

RUN set -eux; \
    apk add --no-cache --virtual .build-deps $PHPIZE_DEPS; \
    pecl install redis brotli-${BROTLI_VERSION}; \
    docker-php-ext-enable redis brotli; \
    apk del .build-deps; \
    rm -rf /tmp/pear

ADD --checksum=sha256:${WP_CLI_SHA256} \
    https://github.com/wp-cli/wp-cli/releases/download/v${WP_CLI_VERSION}/wp-cli-${WP_CLI_VERSION}.phar \
    /usr/local/bin/wp
RUN chmod 0755 /usr/local/bin/wp

# Code WPGenie loads into sites lives here, read-only and outside every
# docroot: a compromised plugin can't alter it, and an image upgrade updates
# it for all sites. Sites only get one-line wrappers that require these files.
ADD --checksum=sha256:${REDIS_CACHE_SHA256} \
    https://downloads.wordpress.org/plugin/redis-cache.${REDIS_CACHE_VERSION}.zip /tmp/redis-cache.zip
RUN set -eux; \
    mkdir -p /usr/local/share/wpgenie; \
    unzip -p /tmp/redis-cache.zip redis-cache/includes/object-cache.php > /usr/local/share/wpgenie/object-cache.php; \
    grep -q "function wp_cache_init" /usr/local/share/wpgenie/object-cache.php; \
    rm /tmp/redis-cache.zip
COPY page-cache.php /usr/local/share/wpgenie/page-cache.php
COPY smtp.php /usr/local/share/wpgenie/smtp.php
COPY profile.php /usr/local/share/wpgenie/profile.php
COPY images.php image-convert.php convert-images.php cdn.php offload.php branding.php optimize.php maintenance.php /usr/local/share/wpgenie/

COPY php.ini  /usr/local/etc/php/conf.d/zz-wpgenie.ini
COPY pool.conf /usr/local/etc/php-fpm.d/zz-wpgenie.conf
COPY jail.ini /usr/local/etc/php/jail.d/zz-jail.ini
RUN chmod -R a+rX,go-w /usr/local/share/wpgenie /usr/local/etc/php/jail.d

# The container root filesystem is read-only at runtime; point every cache
# at the tmpfs. WPG_MAX_CHILDREN is always set by WPGenie; the default only
# keeps a hand-started container bootable.
ENV WP_CLI_CACHE_DIR=/tmp/wp-cli-cache \
    WP_CLI_PACKAGES_DIR=/tmp/wp-cli-packages \
    HOME=/tmp \
    WPG_MAX_CHILDREN=4 \
    WPG_MEMORY_LIMIT=256M \
    WPG_MAX_EXECUTION_TIME=60 \
    WPG_REQUEST_TIMEOUT=120s \
    WPG_UPLOAD_MAX=64M \
    WPG_POST_MAX=64M \
    WPG_MAX_INPUT_VARS=5000

USER 82:82
