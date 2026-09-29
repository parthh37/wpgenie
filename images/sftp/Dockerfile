# WPGenie SFTP: one OpenSSH server for every site's SFTP logins. Each login
# is chrooted to its site's directory and can only use SFTP (no shell, no
# forwarding). Accounts come from the panel (/config, read-only); host keys
# persist in /hostkeys. Built by the daemon the first time SFTP is used.
FROM alpine:3.22
RUN apk add --no-cache openssh-server \
    && cp /etc/passwd /etc/passwd.base && cp /etc/group /etc/group.base && cp /etc/shadow /etc/shadow.base \
    && rm -f /etc/ssh/ssh_host_*
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/wpg-sftp
COPY reload.sh /usr/local/bin/wpg-sftp-reload
RUN chmod 0755 /usr/local/bin/wpg-sftp /usr/local/bin/wpg-sftp-reload && /usr/sbin/sshd -t -f /etc/ssh/sshd_config -h /dev/null 2>&1 | grep -v "no hostkeys" || true
EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/wpg-sftp"]
