#!/bin/sh
# Deterministic configure flow for the stub harness. It exercises the full
# contract without any real credential:
#   - reads the seeded previous configuration (if any) and echoes it, proving
#     the control plane's seed landed before the command ran, and that the seed
#     carries secret metadata but never a secret value
#   - reports whether $PREV_STUB_TOKEN and $STUB_CONFIGURE_KEEP are set: the
#     first is how a previous run's value is offered (a proxy-swapped sentinel,
#     not the credential), the second is what makes this run keep it
#   - writes a fixed secret and file to the configure output path, or keeps the
#     previously stored secret when STUB_CONFIGURE_KEEP is set and there is one
#   - exits 0, or STUB_CONFIGURE_EXIT to exercise the failure path
set -eu
echo "stub configure: previous config was:"
cat /run/discobox/configure/harness-previous-config.json 2>/dev/null || echo "(none)"
echo ""
if [ -n "${PREV_STUB_TOKEN:-}" ]; then
	echo "stub configure: PREV_STUB_TOKEN is set (a sentinel, not the credential)"
else
	echo "stub configure: PREV_STUB_TOKEN is unset"
fi
echo "stub configure: STUB_CONFIGURE_KEEP=${STUB_CONFIGURE_KEEP:-}"
# sandbox-agent creates this directory for the sandbox user in config mode;
# /run/discobox itself is root-owned and stays that way.
mkdir -p /run/discobox/configure
# Keeping is conditional on there actually being something to keep: the first
# configure of a KEEP-baked image has no previous secret, and claiming
# usePrevious then is a commit error. A real harness makes the same check before
# offering "keep the existing credential".
if [ -n "${STUB_CONFIGURE_KEEP:-}" ] && [ -n "${PREV_STUB_TOKEN:-}" ]; then
	# Keep what the control plane already holds, handling no credential at all.
	cat > /run/discobox/configure/harness-configure.json <<'JSON'
{"secrets":[{"envName":"STUB_TOKEN","name":"stub-token","type":"bearer","usePrevious":true}],"files":[{"path":"stub.json","content":"hello"}]}
JSON
else
	cat > /run/discobox/configure/harness-configure.json <<'JSON'
{"secrets":[{"envName":"STUB_TOKEN","name":"stub-token","type":"bearer","value":{"token":"s3cr3t"}}],"files":[{"path":"stub.json","content":"hello"}]}
JSON
fi
echo "stub configure: done"
exit "${STUB_CONFIGURE_EXIT:-0}"
