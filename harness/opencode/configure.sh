#!/bin/sh
set -e

read_secret() {
	printf '%s' "$1" >&2
	stty -echo 2>/dev/null || true
	IFS= read -r value
	stty echo 2>/dev/null || true
	echo >&2
	printf '%s' "$value"
}

ANTHROPIC_API_KEY="$(read_secret 'Enter an Anthropic API key, or press Enter to skip: ')"
OPENAI_API_KEY="$(read_secret 'Enter an OpenAI API key, or press Enter to skip: ')"
if [ -z "$ANTHROPIC_API_KEY" ] && [ -z "$OPENAI_API_KEY" ]; then
	echo "At least one provider API key is required." >&2
	exit 1
fi

mkdir -p /run/discobox
OPENCODE_CONFIGURE_ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" OPENCODE_CONFIGURE_OPENAI_API_KEY="$OPENAI_API_KEY" node <<'NODE_EOF'
const fs = require('fs');
const secrets = [];
if (process.env.OPENCODE_CONFIGURE_ANTHROPIC_API_KEY) secrets.push({ envName: 'ANTHROPIC_API_KEY', name: 'Anthropic API key', type: 'bearer', value: { token: process.env.OPENCODE_CONFIGURE_ANTHROPIC_API_KEY } });
if (process.env.OPENCODE_CONFIGURE_OPENAI_API_KEY) secrets.push({ envName: 'OPENAI_API_KEY', name: 'OpenAI API key', type: 'bearer', value: { token: process.env.OPENCODE_CONFIGURE_OPENAI_API_KEY } });
fs.writeFileSync('/run/discobox/harness-configure.json', JSON.stringify({ files: [], secrets }));
NODE_EOF
echo "OpenCode configuration complete."
