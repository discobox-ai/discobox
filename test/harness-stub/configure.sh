#!/bin/sh
# Deterministic configure flow for the stub harness. It exercises the full
# contract without any real credential:
#   - reads the seeded previous configuration (if any) and echoes it, proving
#     the control plane's seed landed before the command ran, and that the seed
#     carries secret metadata but never a secret value
#   - reports whether $PREV_STUB_TOKEN is set, which is how a previous run's
#     value is offered (a proxy-swapped sentinel, not the credential)
#   - writes a fixed secret and file to the configure output path, or keeps the
#     previously stored secret when STUB_CONFIGURE_KEEP is set
#   - exits 0, or STUB_CONFIGURE_EXIT to exercise the failure path
set -eu
echo "stub configure: previous config was:"
cat /run/discobox/harness-previous-config.json 2>/dev/null || echo "(none)"
echo ""
if [ -n "${PREV_STUB_TOKEN:-}" ]; then
	echo "stub configure: PREV_STUB_TOKEN is set (a sentinel, not the credential)"
else
	echo "stub configure: PREV_STUB_TOKEN is unset"
fi
mkdir -p /run/discobox
if [ -n "${STUB_CONFIGURE_KEEP:-}" ]; then
	# Keep what the control plane already holds, handling no credential at all.
	cat > /run/discobox/harness-configure.json <<'JSON'
{"secrets":[{"envName":"STUB_TOKEN","name":"stub-token","type":"bearer","usePrevious":true}],"files":[{"path":"stub.json","content":"hello"}]}
JSON
else
	cat > /run/discobox/harness-configure.json <<'JSON'
{"secrets":[{"envName":"STUB_TOKEN","name":"stub-token","type":"bearer","value":{"token":"s3cr3t"}}],"files":[{"path":"stub.json","content":"hello"}]}
JSON
fi
echo "stub configure: done"
exit "${STUB_CONFIGURE_EXIT:-0}"
