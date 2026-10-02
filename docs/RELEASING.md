# Releasing

Installed WPGenie servers update themselves only from releases signed with the key whose public
half is compiled into the binary (`internal/updater/release.pub`). The installer checks the same
signature. With an empty `release.pub`, self-update is disabled and the installer warns.

## One-time: create the signing key

```bash
make release-key
```

This writes `internal/updater/release.pub` (commit it) and `release-signing.key` (git-ignored):

1. Add the contents of `release-signing.key` as the repository secret `RELEASE_SIGNING_KEY`
   (Settings → Secrets and variables → Actions).
2. Keep an offline backup of it (a password manager or hardware-encrypted storage), then delete
   the local file. Whoever holds this key can push code to every WPGenie server.

Losing the key means shipping a release with a new `release.pub`, which servers must install by
hand (re-run `install.sh`); old binaries only trust the old key. If the key leaks, do the same
immediately.

## Every release

```bash
git tag v0.3.0 && git push origin v0.3.0
```

The release workflow builds `wpgenie_<tag>_linux_{amd64,arm64}.tar.gz` (binary, `deploy/`,
`images/`), writes `checksums.txt`, signs it into `checksums.txt.sig` and verifies the signature
against the committed `release.pub` before publishing. A release that installs would reject is
never published.

Drafting the release in the GitHub UI instead (with a new tag) works too: the workflow attaches
the signed artifacts to the existing release. Until that run finishes, the release has no assets
and servers that check for updates will fail to download it, so prefer pushing the tag.

## Releases that change the host

Self-update swaps the binary, `/opt/wpgenie` and the systemd unit, rebuilds the PHP image and runs
`docker compose up -d`. It does **not** run the installer's host steps: creating system users,
installing extra systemd units (such as `var-lib-wpgenie-sites.nosymfollow.mount`) or writing new
`infra.env` values. A release that needs any of those must say so in its notes and be installed by
re-running `install.sh`; if it is self-updated anyway, the new compose file fails to start and the
applier rolls back to the running version.
