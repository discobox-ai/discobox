#!/bin/bash
#---
# name: Discobox API
# description: The full dev loop — discobox-server with hot reload, plus the development image watcher
#---

set -euo pipefail

export PORT="${PORT:-8080}"
# The endpoint is named here rather than declared: `task dev` binds only the
# local socket, which nothing outside this container can reach. It reads this
# from the environment (Taskfile `dev:server`), which is why exporting it here
# works. Once the server is listening on a TCP port the sandbox finds and
# forwards it on its own (ADR 0046), which is why a service declares no port.
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

# `dev` rather than `dev:server`: it runs the development image watcher
# alongside the server, so the pool, sandbox and harness images this server
# hands out are rebuilt from the tree it is being edited in. A server running
# against stale images is the confusing half of the dev loop.
#
# Relative paths above resolve against the repository root, because that is
# where a service runs: the directory its own declaration was found in.
exec go tool task dev
