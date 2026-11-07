#!/bin/sh
# The CLI is still where install-cli.sh put it, and still the command
# /usr/local/bin has; a box provisioned before that, whose CLI went to npm's
# default prefix, is installed again.
exec /usr/local/libexec/discobox/npm-global check @anthropic-ai/claude-code
