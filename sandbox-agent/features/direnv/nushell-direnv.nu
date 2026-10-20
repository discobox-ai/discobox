# direnv for nushell, installed in nushell's vendor autoload directory so every
# user's nu loads it after their own config. nu has no `direnv hook`; this is
# the pre-prompt hook direnv's nushell integration is built from.
#
# A shell discobox-shell starts already has the starting directory's .envrc
# loaded (ADR 0138); this is what reloads it on a `cd`. direnv hands PATH back
# as a string, and nu wants a list.
$env.config.hooks.pre_prompt = (
    $env.config.hooks.pre_prompt? | default [] | append {||
        if (which direnv | is-empty) { return }
        direnv export json | from json | default {} | load-env
        if ($env.PATH | describe) == "string" {
            $env.PATH = ($env.PATH | split row (char esep))
        }
    }
)
