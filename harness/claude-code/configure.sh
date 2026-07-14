#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# claude-code Definition). Lets the user complete Claude Code's interactive
# onboarding (theme selection and authentication), then captures the resulting
# theme/settings files and credentials into /run/discobox/harness-configure.json
# for discobox to apply to the new HarnessConfig.
set -e

echo "Starting Claude Code so you can complete its setup (theme + authentication)."
echo "When you are done, exit Claude Code (Ctrl-C, or /exit) to continue."
echo

claude || true

CREDENTIALS_FILE="$HOME/.claude/.credentials.json"
API_KEY=""
if [ ! -f "$CREDENTIALS_FILE" ]; then
	echo
	echo "No Claude subscription session was detected."
	printf 'Enter an Anthropic API key to use instead, or press Enter to skip: '
	stty -echo 2>/dev/null || true
	read -r API_KEY
	stty echo 2>/dev/null || true
	echo
fi

CLAUDE_CONFIGURE_CREDENTIALS_FILE="$CREDENTIALS_FILE" CLAUDE_CONFIGURE_API_KEY="$API_KEY" node <<'NODE_EOF'
const fs = require('fs');
const os = require('os');
const path = require('path');

const home = os.homedir();
const files = [];
const secrets = [];

function readIfExists(p) {
	try {
		return fs.readFileSync(p, 'utf8');
	} catch (err) {
		return null;
	}
}

const claudeJSON = readIfExists(path.join(home, '.claude.json'));
if (claudeJSON !== null) {
	files.push({ path: '.claude.json', content: claudeJSON });
}

const settingsJSON = readIfExists(path.join(home, '.claude', 'settings.json'));
if (settingsJSON !== null) {
	files.push({ path: '.claude/settings.json', content: settingsJSON });
}

const credentialsJSON = readIfExists(process.env.CLAUDE_CONFIGURE_CREDENTIALS_FILE || '');
if (credentialsJSON !== null) {
	files.push({ path: '.claude/.credentials.json', content: credentialsJSON, createOnly: false });
} else if (process.env.CLAUDE_CONFIGURE_API_KEY) {
	secrets.push({
		envName: 'ANTHROPIC_API_KEY',
		name: 'Anthropic API key',
		type: 'bearer',
		value: { token: process.env.CLAUDE_CONFIGURE_API_KEY },
	});
}

fs.mkdirSync('/run/discobox', { recursive: true });
fs.writeFileSync('/run/discobox/harness-configure.json', JSON.stringify({ files, secrets }));
NODE_EOF

echo "Configure complete."
