# Authoritative BIND for the rift e2e. Named runs in the foreground; the zone
# file lives on a writable path because a dynamic-update zone needs a journal
# beside it.
FROM internetsystemsconsortium/bind9:9.20

COPY named.conf /etc/bind/named.conf
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
