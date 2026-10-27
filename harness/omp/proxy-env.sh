# shellcheck shell=sh # Sourced by /etc/profile and the image's scripts, never run.
# Installed as /etc/profile.d/omp-proxy.sh, and sourced by the image's
# launcher and configure command, which do not all run from a login shell.
#
# omp's Anthropic transport is its own HTTP client, and takes a proxy from
# PI_PROXY (or PI_PROXY_<PROVIDER>) alone: given only HTTPS_PROXY it connects
# direct, which a sandbox's egress does not allow, and every Anthropic request
# hangs into "Connection error." So PI_PROXY follows HTTPS_PROXY, which the pool
# owns, rather than naming the egress proxy a second time — a value a user set
# for themselves is left alone (ADR 26-10-14-718 §4).
if [ -n "${HTTPS_PROXY:-}" ] && [ -z "${PI_PROXY:-}" ]; then
	export PI_PROXY="$HTTPS_PROXY"
fi
