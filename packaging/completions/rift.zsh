#compdef rift
# zsh completion for rift.
#   Install as _rift somewhere on $fpath, e.g.
#   /usr/share/zsh/site-functions/_rift or ~/.local/share/zsh/site-functions/_rift
#
# Flag/argument surface mirrors cli/src/args.ts and cli/src/constants.ts.

_rift() {
    _arguments -s -S \
        '(- *)'{-h,--help}'[print usage and exit]' \
        '(- *)'{-v,--version}'[print version and exit]' \
        '--token[gateway auth token]:token:' \
        '--server[gateway ws/wss URL]:url:' \
        '--host[local host to forward to]:host:_hosts' \
        '--log-level[log verbosity]:level:(debug info warn error silent)' \
        '--insecure[skip TLS certificate verification (wss only)]' \
        '1:protocol:(http)' \
        '2:port:' \
        '3:subdomain:' \
        && return 0
}

_rift "$@"
