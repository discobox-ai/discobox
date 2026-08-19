#!/bin/bash
#---
# name: Discobox API
# description: Runs discobox-server with watchnbuild hot reload on port 8080
# order: 10
# http: 8080
# path: /docs
#---

set -euo pipefail

export PORT="${PORT:-8080}"
# This service publishes the API over HTTP (the `http:`/`path:` headers above),
# so it names the TCP endpoint itself. `task dev:server` binds only the local
# socket, which nothing outside this container can reach.
export DISCOBOX_SERVER_LISTEN="${DISCOBOX_SERVER_LISTEN:-http://127.0.0.1:$PORT}"
export DISCOBOX_DATA_DIR="${DISCOBOX_DATA_DIR:-$PWD/.tmp/discobox/data}"
export DISCOBOX_CONFIG_DIR="${DISCOBOX_CONFIG_DIR:-$PWD/.tmp/discobox/config}"
export DISCOBOX_CACHE_DIR="${DISCOBOX_CACHE_DIR:-$PWD/.tmp/discobox/cache}"
export DISCOBOX_STATE_DIR="${DISCOBOX_STATE_DIR:-$PWD/.tmp/discobox/state}"
export DATABASE_DSN="${DATABASE_DSN:-sqlite3://$PWD/.tmp/discobox/discobox.db}"
export OTEL_METRICS_EXPORTER="${OTEL_METRICS_EXPORTER:-otlp}"
export OTEL_EXPORTER_OTLP_ENDPOINT="${OTEL_EXPORTER_OTLP_ENDPOINT:-http://localhost:4318}"
export OTEL_EXPORTER_OTLP_PROTOCOL="${OTEL_EXPORTER_OTLP_PROTOCOL:-http/protobuf}"
export OTEL_METRIC_EXPORT_INTERVAL="${OTEL_METRIC_EXPORT_INTERVAL:-1000}"

mkdir -p "$DISCOBOX_DATA_DIR" "$DISCOBOX_CONFIG_DIR" "$DISCOBOX_CACHE_DIR" "$DISCOBOX_STATE_DIR"

exec go tool task dev:server
