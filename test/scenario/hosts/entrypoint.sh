#!/bin/sh
# The container's start, as root: the authorized key; an ifb device so
# the harness can delay the inbound direction too; the chains it puts
# faults in (TT-FREEZE: half-open now, TT-HALFOPEN: the connections a
# network change left behind); SYNs to port 2222 dropped (a host that
# times out); an sshd on 2223 with key auth off (a host that wants a
# password); then sshd.
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
for c in TT-HALFOPEN TT-FREEZE; do
	iptables -N "$c"
	iptables -A INPUT -j "$c"
	iptables -A OUTPUT -j "$c"
done
iptables -A INPUT -p tcp --dport 2222 --syn -j DROP
ssh-keygen -A >/dev/null
/usr/sbin/sshd -p 2223 -o PubkeyAuthentication=no -o PasswordAuthentication=yes
exec /usr/sbin/sshd -D -e
