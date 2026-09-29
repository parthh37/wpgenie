# Request-body WAF rules

Coraza (built into WPGenie's Caddy image, `images/caddy`) runs the OWASP Core
Rule Set, which the Coraza module embeds. WordPress needs rule exclusions or
it blocks ordinary things (post content with HTML, passwords with quotes).
These two files are the CRS project's
[WordPress rule exclusions plugin](https://github.com/coreruleset/wordpress-rule-exclusions-plugin)
v1.2.0, commit `0f52656`, unmodified (Apache License 2.0, see
`LICENSE-crs-wordpress-plugin`). WPGenie's own settings are in `wpgenie.conf`.

To update: copy the two `plugins/*.conf` files from a new release over these and
run `WPGENIE_TEST_DOCKER=1 go test ./internal/proxy -run WAF`.
