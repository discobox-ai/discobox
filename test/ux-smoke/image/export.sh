#!/bin/sh
set -eu
if [ "${1-}" = handoff ]; then
    cat >/dev/null
    rm -f "$HOME/.ux-smoke-token"
    printf '%s\n' '{"version":1,"state":"applied"}'
    exit
fi
input=$(cat)
if printf '%s' "$input" | jq -e '.baselineOnly' >/dev/null; then
    printf '%s' "$input" | jq '{version:1,files:((.baseline.files // [] | map(select(.path == ".ux-smoke.json")))),credentials:[]}'
else
    credentials='[]'
    if [ -f "$HOME/.ux-smoke-token" ]; then
        credentials=$(jq -n --rawfile token "$HOME/.ux-smoke-token" \
            '[{provider:"synthetic",envName:"UX_SMOKE_TOKEN",type:"token",localId:".ux-smoke-token",generation:"synthetic-v1",value:{token:$token}}]')
    fi
    jq -n --argjson credentials "$credentials" --rawfile settings "$HOME/.ux-smoke.json" \
        '{version:1,files:[{path:".ux-smoke.json",content:$settings}],credentials:$credentials}'
fi
