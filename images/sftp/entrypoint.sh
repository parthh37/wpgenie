#!/bin/sh
set -eu
# Host keys persist, so clients don't see a new server after a restart.
for t in ed25519 rsa; do
	[ -f "/hostkeys/ssh_host_${t}_key" ] || ssh-keygen -q -t "$t" -N '' -f "/hostkeys/ssh_host_${t}_key"
done
/usr/local/bin/wpg-sftp-reload
exec /usr/sbin/sshd -D -e
