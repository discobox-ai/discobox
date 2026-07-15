#!/bin/sh
set -e

printf 'Enter an OpenAI API key for Codex: '
stty -echo 2>/dev/null || true
read -r OPENAI_API_KEY
stty echo 2>/dev/null || true
echo
if [ -z "$OPENAI_API_KEY" ]; then
	echo "An OpenAI API key is required." >&2
	exit 1
fi

mkdir -p /run/discobox
CODEX_CONFIGURE_OPENAI_API_KEY="$OPENAI_API_KEY" node <<'NODE_EOF'
const fs = require('fs');
fs.writeFileSync('/run/discobox/harness-configure.json', JSON.stringify({
  files: [],
  secrets: [{ envName: 'OPENAI_API_KEY', name: 'OpenAI API key', type: 'bearer', value: { token: process.env.CODEX_CONFIGURE_OPENAI_API_KEY } }],
}));
NODE_EOF
echo "Codex configuration complete."
