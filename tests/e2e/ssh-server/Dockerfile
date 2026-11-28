# Test SSH server for the kind e2e of SSH executors (#204): a plain Debian with openssh-server, sudo and python3
# (Ansible targets). It is NOT enrolled: the e2e applies the output of scripts/ssh-enrol.sh to it, as an admin would.
FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server sudo python3 procps \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd && rm -f /etc/ssh/ssh_host_*key*
# host keys are made at first start (each pod gets its own); logs to stderr with the accepted certificate's key ID
CMD ["/bin/sh", "-c", "ssh-keygen -A >/dev/null && exec /usr/sbin/sshd -D -e -o LogLevel=VERBOSE"]
