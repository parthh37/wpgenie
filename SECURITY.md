# Security Policy

WPGenie hosts other people's websites, so we treat security reports as top priority.

## Reporting a vulnerability

**Please do not open a public issue.** Use GitHub's private
[security advisory form](https://github.com/parthh37/wpgenie/security/advisories/new).

Include affected version/commit, reproduction steps, and impact. We aim to acknowledge within
72 hours and to ship a fix or mitigation for critical issues within 14 days.

## Scope

In scope: the `wpgenie` daemon, installer, generated Caddy configuration, the PHP runtime image,
and default hardening. Vulnerabilities in WordPress core, plugins or themes themselves should be
reported to their maintainers (WPGenie's job is to contain them).

## Supported versions

Until 1.0, only the latest release receives security fixes.
