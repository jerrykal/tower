#!/bin/sh
# The container's start, as root: the authorized key, an ifb device so
# the harness can delay the inbound direction too, then sshd.
set -e
install -d -m 700 -o tt -g tt /home/tt/.ssh
printf '%s\n' "$TT_PUBKEY" > /home/tt/.ssh/authorized_keys
chown tt:tt /home/tt/.ssh/authorized_keys
chmod 600 /home/tt/.ssh/authorized_keys
printf '%s\n' "$TT_ENV" > /etc/tt-env-path
ip link add ifb0 type ifb
ip link set ifb0 up
tc qdisc add dev eth0 handle ffff: ingress
tc filter add dev eth0 parent ffff: matchall action mirred egress redirect dev ifb0
ssh-keygen -A >/dev/null
exec /usr/sbin/sshd -D -e
