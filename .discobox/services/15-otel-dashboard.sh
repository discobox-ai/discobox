#!/bin/bash
#---
# name: OTEL Metrics Dashboard
# description: Runs the Aspire Dashboard OTLP receiver and metrics UI on port 18888
#---

set -euo pipefail

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
