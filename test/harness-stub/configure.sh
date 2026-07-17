#!/bin/sh
# Deterministic configure flow for the stub harness. It exercises the full
# contract without any real credential:
#   - reads the seeded previous configuration (if any) and echoes it, proving
#     the control plane's seed landed before the command ran
#   - writes a fixed secret and file to the configure output path
#   - exits 0, or STUB_CONFIGURE_EXIT to exercise the failure path
set -eu
echo "stub configure: previous config was:"
cat /run/discobox/harness-previous-config.json 2>/dev/null || echo "(none)"
echo ""
mkdir -p /run/discobox
cat > /run/discobox/harness-configure.json <<'JSON'
{"secrets":[{"envName":"STUB_TOKEN","name":"stub-token","type":"bearer","value":{"token":"s3cr3t"}}],"files":[{"path":"stub.json","content":"hello"}]}
JSON
echo "stub configure: done"
exit "${STUB_CONFIGURE_EXIT:-0}"
