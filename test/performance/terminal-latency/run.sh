#!/usr/bin/env bash
set -euo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$repo_root"

# Empty means "wherever this machine's discobox talks to": `task dev` binds the
# local socket and nothing else, and the path of that socket is the CLI's to
# resolve, not this script's to recompute.
server="${DISCOBOX_TERMINAL_LATENCY_SERVER:-}"
project="${DISCOBOX_TERMINAL_LATENCY_PROJECT:-default}"
samples="${DISCOBOX_TERMINAL_LATENCY_SAMPLES:-100}"
interval="${DISCOBOX_TERMINAL_LATENCY_INTERVAL:-20ms}"
timeout="${DISCOBOX_TERMINAL_LATENCY_TIMEOUT:-5s}"
settle="${DISCOBOX_TERMINAL_LATENCY_SETTLE:-250ms}"
modes="${DISCOBOX_TERMINAL_LATENCY_MODES:-transport,direct}"
profiles="${DISCOBOX_TERMINAL_LATENCY_PROFILES:-quiet,spinner,screen}"
spinner_hz="${DISCOBOX_TERMINAL_LATENCY_SPINNER_HZ:-30}"
spinner_bytes="${DISCOBOX_TERMINAL_LATENCY_SPINNER_BYTES:-128}"
screen_hz="${DISCOBOX_TERMINAL_LATENCY_SCREEN_HZ:-30}"
screen_bytes="${DISCOBOX_TERMINAL_LATENCY_SCREEN_BYTES:-4800}"
cpu_vcpus="${DISCOBOX_TERMINAL_LATENCY_CPU_VCPUS:-1}"
keep="${DISCOBOX_TERMINAL_LATENCY_KEEP:-0}"
run_id="$(date -u +%Y%m%dt%H%M%Sz)-$$"
resource_id="$(date -u +%H%M%S)-$$"
output_dir="${DISCOBOX_TERMINAL_LATENCY_OUTPUT_DIR:-$repo_root/.tmp/terminal-latency/$run_id}"
image="discobox-terminal-latency:local"
harness_name="latency-$resource_id"
sandbox_name=""
container_id=""

for command in docker go jq timeout tmux; do
	if ! command -v "$command" >/dev/null 2>&1; then
		echo "terminal latency: $command is required" >&2
		exit 1
	fi
done
if ! docker info >/dev/null 2>&1; then
	echo "terminal latency: the Docker daemon is unavailable" >&2
	exit 1
fi
if ! [[ "$samples" =~ ^[1-9][0-9]*$ ]]; then
	echo "terminal latency: DISCOBOX_TERMINAL_LATENCY_SAMPLES must be a positive integer" >&2
	exit 1
fi
IFS=',' read -r -a requested_modes <<<"$modes"
if [ "${#requested_modes[@]}" -eq 0 ]; then
	echo "terminal latency: DISCOBOX_TERMINAL_LATENCY_MODES must not be empty" >&2
	exit 1
fi
for mode in "${requested_modes[@]}"; do
	case "$mode" in
		transport | direct | tui) ;;
		*)
			echo "terminal latency: unsupported mode '$mode' (use transport,direct,tui)" >&2
			exit 1
			;;
	esac
done

IFS=',' read -r -a requested_profiles <<<"$profiles"
if [ "${#requested_profiles[@]}" -eq 0 ]; then
	echo "terminal latency: DISCOBOX_TERMINAL_LATENCY_PROFILES must not be empty" >&2
	exit 1
fi
for profile in "${requested_profiles[@]}"; do
	case "$profile" in
		quiet | spinner | screen) ;;
		*)
			echo "terminal latency: unsupported profile '$profile' (use quiet,spinner,screen)" >&2
			exit 1
			;;
	esac
done
for setting in "$spinner_hz" "$spinner_bytes" "$screen_hz" "$screen_bytes"; do
	if ! [[ "$setting" =~ ^[1-9][0-9]*$ ]]; then
		echo "terminal latency: output rates and frame sizes must be positive integers" >&2
		exit 1
	fi
done

mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"

echo "Building the current CLI and deterministic latency harness image..."
go tool task build:cli
go tool task build:terminal-latency-image

cli=("$repo_root/build/discobox")
if [ -n "$server" ]; then
	cli+=(--server "$server")
fi
cli+=(--project "$project" --auto-start-server=false --output json)
if [ -n "${DISCOBOX_TOKEN:-}" ]; then
	cli+=(--token "$DISCOBOX_TOKEN")
fi

# Reachability is checked through the CLI rather than curl: the server may be
# listening on a unix socket, an iroh endpoint, or a URL, and discobox is what
# resolves all three. --auto-start-server=false above keeps a missing server a
# failure instead of launching one.
if ! "${cli[@]}" admin harness ls >/dev/null 2>&1; then
	echo "terminal latency: no development server at ${server:-the default endpoint}; start one with 'go tool task dev'" >&2
	exit 1
fi

harness_id=""
sandbox_id=""
kept_sandbox_ids=()

wait_for_sandbox_deletion() {
	local id="$1"
	local attempt
	for attempt in {1..60}; do
		if ! "${cli[@]}" admin box get "$id" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

cleanup() {
	local status=$?
	trap - EXIT
	if [ "$keep" = "1" ]; then
		if [ -n "$sandbox_id" ]; then
			kept_sandbox_ids+=("$sandbox_id")
		fi
		echo "Keeping sandboxes ${kept_sandbox_ids[*]:-(none)} and harness $harness_id (DISCOBOX_TERMINAL_LATENCY_KEEP=1)." >&2
		exit "$status"
	fi

	if [ -n "$sandbox_id" ]; then
		"${cli[@]}" admin box delete "$sandbox_id" >/dev/null 2>&1 || true
		if ! wait_for_sandbox_deletion "$sandbox_id"; then
			echo "terminal latency: sandbox $sandbox_id is still deleting; harness cleanup may need a retry" >&2
		fi
	fi
	if [ -n "$harness_id" ]; then
		configure_sandbox_id="$("${cli[@]}" admin harness get "$harness_id" 2>/dev/null | jq -r '.configureSandboxId // empty' || true)"
		if [ -n "$configure_sandbox_id" ] && [ "$configure_sandbox_id" != "$sandbox_id" ]; then
			"${cli[@]}" admin box delete "$configure_sandbox_id" >/dev/null 2>&1 || true
			wait_for_sandbox_deletion "$configure_sandbox_id" || true
		fi
		"${cli[@]}" admin harness deconfigure "$harness_id" >/dev/null 2>&1 || true
		"${cli[@]}" admin harness delete "$harness_id" >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup EXIT

delete_probe_sandbox() {
	local id="$sandbox_id"
	if [ -z "$id" ]; then
		return 0
	fi
	if [ "$keep" = "1" ]; then
		kept_sandbox_ids+=("$id")
		sandbox_id=""
		sandbox_name=""
		container_id=""
		return 0
	fi
	"${cli[@]}" admin box delete "$id" >/dev/null
	if ! wait_for_sandbox_deletion "$id"; then
		echo "terminal latency: sandbox $id did not finish deleting" >&2
		return 1
	fi
	sandbox_id=""
	sandbox_name=""
	container_id=""
}

create_probe_sandbox() {
	local mode="$1"
	local profile="$2"
	local sandbox_json
	sandbox_name="$harness_name-$mode-$profile"
	local sandbox_args=(
		admin box create
		--name "$sandbox_name"
		--harness-config "$harness_id"
		--cpu-vcpus "$cpu_vcpus"
		--env "DISCOBOX_LATENCY_OUTPUT_PROFILE=$profile"
		--wait
		--wait-timeout 2m30s
	)
	case "$profile" in
		spinner)
			sandbox_args+=(--env "DISCOBOX_LATENCY_OUTPUT_HZ=$spinner_hz")
			sandbox_args+=(--env "DISCOBOX_LATENCY_OUTPUT_BYTES=$spinner_bytes")
			;;
		screen)
			sandbox_args+=(--env "DISCOBOX_LATENCY_OUTPUT_HZ=$screen_hz")
			sandbox_args+=(--env "DISCOBOX_LATENCY_OUTPUT_BYTES=$screen_bytes")
			;;
	esac
	echo "Creating a $cpu_vcpus vCPU disposable sandbox for $mode/$profile..."
	sandbox_json="$("${cli[@]}" "${sandbox_args[@]}")"
	sandbox_id="$(jq -er '.id' <<<"$sandbox_json")"
	printf '%s\n' "$sandbox_json" >"$output_dir/sandbox-$mode-$profile.json"

	container_id=""
	for _ in {1..30}; do
		container_id="$(docker ps -q --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)"
		if [ -n "$container_id" ]; then
			return 0
		fi
		sleep 0.2
	done
	echo "terminal latency: could not resolve the Docker container for $sandbox_id" >&2
	return 1
}

profile_settings() {
	case "$1" in
		quiet)
			load_hz=0
			load_bytes=0
			;;
		spinner)
			load_hz="$spinner_hz"
			load_bytes="$spinner_bytes"
			;;
		screen)
			load_hz="$screen_hz"
			load_bytes="$screen_bytes"
			;;
	esac
}

echo "Registering disposable harness $harness_name..."
harness_json="$("${cli[@]}" admin harness create \
	--name "$harness_name" \
	--slug "$harness_name" \
	--image "$image")"
harness_id="$(jq -er '.id' <<<"$harness_json")"

timeout 3m "${cli[@]}" admin harness configure "$harness_id" </dev/null >"$output_dir/configure.json"

mode_enabled() {
	case ",$modes," in
		*",$1,"*) return 0 ;;
		*) return 1 ;;
	esac
}

if mode_enabled transport; then
	profile_index=0
	for profile in "${requested_profiles[@]}"; do
		profile_settings "$profile"
		create_probe_sandbox transport "$profile"
		echo "Measuring the annotated resumable transport under $profile output..."
		(
			cd cli
			DISCOBOX_TERMINAL_LATENCY_E2E=1 \
			DISCOBOX_TERMINAL_LATENCY_SERVER="$server" \
			DISCOBOX_TERMINAL_LATENCY_PROJECT="$project" \
			DISCOBOX_TERMINAL_LATENCY_SANDBOX="$sandbox_id" \
			DISCOBOX_TERMINAL_LATENCY_SAMPLES="$samples" \
			DISCOBOX_TERMINAL_LATENCY_SEQUENCE_START="$((10000001 + profile_index * 100000))" \
			DISCOBOX_TERMINAL_LATENCY_INTERVAL="$interval" \
			DISCOBOX_TERMINAL_LATENCY_TIMEOUT="$timeout" \
			DISCOBOX_TERMINAL_LATENCY_SETTLE="$settle" \
			DISCOBOX_TERMINAL_LATENCY_LOAD_PROFILE="$profile" \
			DISCOBOX_TERMINAL_LATENCY_LOAD_HZ="$load_hz" \
			DISCOBOX_TERMINAL_LATENCY_LOAD_BYTES="$load_bytes" \
			DISCOBOX_TERMINAL_LATENCY_REPORT="$output_dir/transport-$profile.json" \
				go test ./internal/cli -run '^TestTerminalLatencyE2E$' -count=1 -v
		)
		delete_probe_sandbox
		profile_index=$((profile_index + 1))
	done
fi

if mode_enabled direct; then
	profile_index=0
	for profile in "${requested_profiles[@]}"; do
		profile_settings "$profile"
		create_probe_sandbox direct "$profile"
		echo "Measuring the direct attach/run hot path through tmux under $profile output..."
		driver_args=(
			--server "$server"
			--project "$project"
			--sandbox "$sandbox_id"
			--sandbox-name "$sandbox_name"
			--cli "$repo_root/build/discobox"
			--mode direct
			--samples "$samples"
			--sequence-start "$((20000001 + profile_index * 100000))"
			--interval "$interval"
			--timeout "$timeout"
			--settle "$settle"
			--container "$container_id"
			--load-profile "$profile"
			--load-hz "$load_hz"
			--load-bytes "$load_bytes"
			--output "$output_dir/direct-$profile.json"
		)
		if [ -n "${DISCOBOX_TOKEN:-}" ]; then
			driver_args+=(--token "$DISCOBOX_TOKEN")
		fi
		go run ./test/performance/terminal-latency "${driver_args[@]}"
		delete_probe_sandbox
		profile_index=$((profile_index + 1))
	done
fi

if mode_enabled tui; then
	create_probe_sandbox tui quiet
	echo "Measuring the optional Bubble Tea TUI client path through tmux..."
	driver_args=(
		--server "$server"
		--project "$project"
		--sandbox "$sandbox_id"
		--sandbox-name "$sandbox_name"
		--cli "$repo_root/build/discobox"
		--mode tui
		--samples "$samples"
		--sequence-start 30000001
		--interval "$interval"
		--timeout "$timeout"
		--settle "$settle"
		--container "$container_id"
		--load-profile quiet
		--output "$output_dir/tui-quiet.json"
	)
	if [ -n "${DISCOBOX_TOKEN:-}" ]; then
		driver_args+=(--token "$DISCOBOX_TOKEN")
	fi
	go run ./test/performance/terminal-latency "${driver_args[@]}"
	delete_probe_sandbox
fi

echo
echo "Terminal latency reports:"
for report in "$output_dir"/*.json; do
	case "$(basename "$report")" in
		transport-*.json)
			jq -r '"  transport/\(.loadProfile)  p50=\(.summary.echoRoundTrip.p50Us / 1000)ms p95=\(.summary.echoRoundTrip.p95Us / 1000)ms p99=\(.summary.echoRoundTrip.p99Us / 1000)ms output=\(.outputBytesPerSecond / 1024 | . * 10 | round / 10)KiB/s"' "$report"
			;;
		direct-*.json|tui-quiet.json)
			jq -r '"  \(.kind)/\(.loadProfile)  p50=\(.summary.p50Us / 1000)ms p95=\(.summary.p95Us / 1000)ms p99=\(.summary.p99Us / 1000)ms output=\(.paneOutputBytesPerSecond / 1024 | . * 10 | round / 10)KiB/s"' "$report"
			;;
	esac
done
echo "  artifacts: $output_dir"
