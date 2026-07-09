# bash completion for rift.
#   Source directly, or drop into a bash-completion directory, e.g.
#   /usr/share/bash-completion/completions/rift or ~/.local/share/bash-completion/completions/rift
#
# Completes the protocol, flags, and --log-level values. Flag/argument surface
# mirrors cli/src/args.ts and cli/src/constants.ts.

_rift() {
    local cur prev
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    COMPREPLY=()

    local flags="--token --server --host --log-level --insecure --version -v --help -h"
    local protocols="http"
    local levels="debug info warn error silent"

    # Value-taking flags: complete their argument, not another flag.
    case "$prev" in
        --log-level)
            mapfile -t COMPREPLY < <(compgen -W "$levels" -- "$cur")
            return 0
            ;;
        --token|--server|--host)
            # Free-form value; nothing sensible to suggest.
            return 0
            ;;
    esac

    if [[ "$cur" == -* ]]; then
        mapfile -t COMPREPLY < <(compgen -W "$flags" -- "$cur")
        return 0
    fi

    # Count positional (non-flag) words seen so far, skipping value-flag args,
    # to decide which positional the cursor is on: 1=protocol, 2=port, 3=subdomain.
    local i pos=0 skip=0
    for ((i = 1; i < COMP_CWORD; i++)); do
        local w="${COMP_WORDS[i]}"
        if (( skip )); then
            skip=0
            continue
        fi
        case "$w" in
            --token|--server|--host|--log-level)
                skip=1
                ;;
            --*=*|-*)
                ;;
            *)
                ((pos++))
                ;;
        esac
    done

    case "$pos" in
        0)
            mapfile -t COMPREPLY < <(compgen -W "$protocols" -- "$cur")
            ;;
        *)
            # port (2nd) and subdomain (3rd) are free-form.
            ;;
    esac
    return 0
}

complete -F _rift rift
