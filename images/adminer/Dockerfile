# WPGenie Adminer: database access on demand. Answers only the WPGenie
# daemon, which proxies it on a site's own domain behind a short-lived
# token and logs in with a temporary database account (see
# internal/adminer). Built by the daemon the first time it is used.
FROM adminer:5.5.1
USER root
COPY wpgenie.php /var/www/html/_wpgenie/adminer/index.php
COPY router.php /var/www/html/_wpgenie/router.php
RUN chmod -R a+rX,go-w /var/www/html/_wpgenie && php -l /var/www/html/_wpgenie/adminer/index.php \
    && php -l /var/www/html/_wpgenie/router.php
USER adminer
# Several requests at once (the built-in server is single-threaded otherwise).
ENV PHP_CLI_SERVER_WORKERS=4
CMD ["php", "-S", "[::]:8080", "-t", "/var/www/html", "/var/www/html/_wpgenie/router.php"]
