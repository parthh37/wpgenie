# WPGenie phpMyAdmin: database access on demand. Answers only the WPGenie
# daemon, which proxies it on a site's own domain behind a short-lived
# token and logs in with a temporary database account (see
# internal/phpmyadmin). Built by the daemon the first time it is used.
# Pinned by digest: the daemon rebuilds this image when these sources
# change, and a bare tag would rebuild on the PHP it first downloaded.
FROM phpmyadmin:5.2.3-fpm-alpine@sha256:6fda6542ee42f679a6701a3e6fc53bd9ae3bc4a67438f8b600d8bb90f46dcfa1
USER root
# The same PHP security floor as images/php (PHP2Shell, CVE-2026-109618/9).
RUN php -r '$floor = ["8.2" => "8.2.35", "8.3" => "8.3.36", "8.4" => "8.4.27", "8.5" => "8.5.12"][PHP_MAJOR_VERSION . "." . PHP_MINOR_VERSION] ?? null; \
    if ($floor === null || version_compare(PHP_VERSION, $floor, "<")) { fwrite(STDERR, "PHP " . PHP_VERSION . " is below the security floor " . ($floor ?? "(unsupported line)") . "\n"); exit(1); }'
# phpMyAdmin is served under /_wpgenie/phpmyadmin/ (the path the daemon
# proxies) through a symlink in the built-in server's document root, so
# its own URLs and cookie paths are right without rewriting anything.
RUN mkdir -p /srv/_wpgenie /opt/wpgenie && ln -s /var/www/html /srv/_wpgenie/phpmyadmin \
    && rm -f /var/www/html/config.sample.inc.php /var/www/html/show_config_errors.php
COPY config.inc.php /etc/phpmyadmin/config.inc.php
COPY router.php /opt/wpgenie/router.php
RUN chmod a+r,go-w /etc/phpmyadmin/config.inc.php /opt/wpgenie/router.php \
    && php -l /etc/phpmyadmin/config.inc.php && php -l /opt/wpgenie/router.php
USER www-data
# The root filesystem is read-only: sessions and phpMyAdmin's cache go to
# the /tmp tmpfs. Imports up to 64 MB (compress bigger dumps: .gz, .zip).
ENV SESSION_SAVE_PATH=/tmp UPLOAD_LIMIT=64M MEMORY_LIMIT=256M MAX_EXECUTION_TIME=300 \
    PHP_CLI_SERVER_WORKERS=4
# Not the image's entrypoint: it writes configuration we don't use.
ENTRYPOINT []
CMD ["php", "-d", "opcache.enable_cli=1", "-S", "[::]:8080", "-t", "/srv", "/opt/wpgenie/router.php"]
