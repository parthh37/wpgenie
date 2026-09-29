#!/bin/sh
# Installs the accounts the panel generated (/config) next to the image's
# system accounts. Each file is replaced atomically. Runs at start and on
# every change, possibly both at once: each run writes its own temporary
# files ($$), so two runs never move each other's.
set -eu
trap 'rm -f /etc/passwd.new.$$ /etc/group.new.$$ /etc/shadow.new.$$' EXIT
for f in passwd group shadow; do
	cat "/etc/$f.base" "/config/$f" >"/etc/$f.new.$$"
	chmod 0644 "/etc/$f.new.$$"
	[ "$f" = shadow ] && chmod 0600 "/etc/$f.new.$$"
	mv "/etc/$f.new.$$" "/etc/$f"
done
