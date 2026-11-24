#!/bin/bash
#---
# features: [docker]
# name: OTEL Metrics Dashboard
# description: Runs the Aspire Dashboard OTLP receiver and metrics UI on port 18888
# ports: 18888
#---

# The dashboard's ports are published by dockerd, as root, so the sandbox's port
# watcher never sees one however plainly this service is what put it there
# (ADR 0076) — hence the declaration above. Only the UI port is declared: the
# OTLP receivers below are pushed to from inside the sandbox, and forwarding
# them to a laptop is not something anyone has wanted.

set -euo pipefail

# Not where creating a container crashes the machine (#1639). Exit 0: it reads
# as exited, not failed.
# shellcheck source=scripts/docker-storage-unsupported.sh
. scripts/docker-storage-unsupported.sh
if reason=$(docker_storage_unsupported); then
	echo "OTEL Metrics Dashboard not started: $reason"
	exit 0
fi

container="discobox-otel-dashboard"
volume="discobox-otel-dashboard"
dashboard_uid="1654"

docker volume create "$volume" >/dev/null
docker run --rm \
	-v "$volume:/keys" \
	busybox:1.36 \
	sh -c "chown -R $dashboard_uid:$dashboard_uid /keys"

exec docker run --rm \
	--name "$container" \
	--mount "type=volume,source=$volume,target=/home/app/.aspnet/DataProtection-Keys" \
	-p 127.0.0.1:18888:18888 \
	-p 127.0.0.1:4317:18889 \
	-p 127.0.0.1:4318:18890 \
	-e DOTNET_DASHBOARD_UNSECURED_ALLOW_ANONYMOUS=true \
	mcr.microsoft.com/dotnet/aspire-dashboard:latest
