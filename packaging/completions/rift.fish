# fish completion for rift.
#   Install into a fish completions dir, e.g.
#   ~/.config/fish/completions/rift.fish
#
# Flag/argument surface mirrors cli/src/args.ts and cli/src/constants.ts.

# No file completion by default; rift takes a protocol, port, and subdomain.
complete -c rift -f

# Value flags.
complete -c rift -l token  -r -d 'gateway auth token'
complete -c rift -l server -r -d 'gateway ws/wss URL'
complete -c rift -l host   -r -d 'local host to forward to'
complete -c rift -l log-level -x -a 'debug info warn error silent' -d 'log verbosity'

# Boolean flags.
complete -c rift -l insecure -d 'skip TLS certificate verification (wss only)'
complete -c rift -s v -l version -d 'print version and exit'
complete -c rift -s h -l help    -d 'print usage and exit'

# First positional token is the protocol (only `rift` seen so far).
complete -c rift -n 'test (count (commandline -opc)) -eq 1' -a 'http' -d 'application protocol'
