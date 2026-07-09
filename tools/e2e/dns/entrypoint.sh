#!/bin/sh
set -eu
# The zone template carries placeholders because the addresses are only known
# once compose has assigned them.
sed -e "s/__BIND_IP__/${RIFT_BIND_IP}/g" \
    -e "s/__CADDY_IP__/${RIFT_CADDY_IP}/g" \
    /zone/db.rift.localtest > /var/lib/bind/db.rift.localtest
chown bind:bind /var/lib/bind/db.rift.localtest /var/lib/bind
exec named -g -c /etc/bind/named.conf -u bind
