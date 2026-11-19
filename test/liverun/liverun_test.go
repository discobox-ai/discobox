// Package liverun_test runs scripts/live-run.sh, the live validation recipe,
// without a provider: its dry run, and its live path against stand-in
// discobox, disco-vm and curl commands that record how they were called.
package liverun_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const key = "bxd_live-run-test-key-must-not-leak"

// stubDiscobox answers the calls the script makes the way the CLI does, and
// keeps what was deleted so a get afterwards fails. It logs each call's
// argv, and copies the provider's @config so the test can read it.
const stubDiscobox = `#!/usr/bin/env bash
set -eu
printf '%s\n' "discobox $*" >>"$STUB/calls"
args=()
for a in "$@"; do case $a in --server|--server=*|unix://*|-o|json) ;; *) args+=("$a") ;; esac; done
set -- "${args[@]}"
case "$1 ${2:-} ${3:-}" in
"admin harness get") echo '{}' ;;
"secret bound --harness") cat "$STUB/harness-binding" 2>/dev/null || echo '{"boundSecrets":[{"use":true,"envVar":"ANTHROPIC_API_KEY","routes":[[{"hosts":["api.anthropic.com"]}]]}]}' ;;
"secret create --name") echo '{"id":"sec_T1"}' ;;
"secret bind sec_T1"|"secret rm sec_T1") ;;
"admin provider list") echo '[]' ;;
"admin box get")
	# $STUB/provision, when there, is the discobox's provisioning state.
	if [ "$4" = sbx_T2 ]; then
		cat "$STUB/second-box" 2>/dev/null || echo '{"runtime":{"state":"running","startedFrom":{"from":"snapshot","snapshot":"snapshot-1"}}}'
	else
		cat "$STUB/provision" 2>/dev/null || echo '{"id":"sbx_T1","runtime":{"agentStatus":{"provision":{"state":"ready"}}}}'
	fi ;;
"admin pool logs") echo "control-plane channel open" ;;
"admin provider create")
	for a in "$@"; do case $a in @*) cp "${a#@}" "$STUB/provider.json"; jq -r ".driver.region" "$STUB/provider.json" >"$STUB/provider-region" ;; esac; done
	echo '{"id":"prov_T1"}' ;;
"admin provider check")
	# The server answers a failed check 200 with its outcome; the CLI prints
	# it and exits non-zero.
	if [ -e "$STUB/provider-unreachable" ]; then
		echo '{"outcome":"unreachable","message":"boxd: exchange API key: Post \"https://app.boxd.sh/api/v1/auth/token\": dial tcp 1.2.3.4:443: connect: network is unreachable: pool agent is not reachable","checkedAt":"2026-11-15T10:00:00Z"}'
		echo 'Error: provider prov_T1 check: unreachable' >&2; exit 1
	fi
	echo '{"outcome":"ok","checkedAt":"2026-11-15T10:00:00Z"}' ;;
"admin pool create") echo '{"id":"pool_T1"}' ;;
"admin pool update") ;;
"admin pool get")
	if [ -e "$STUB/provider-unreachable" ] && [ ! -e "$STUB/pool.deleted" ]; then
		echo '{"state":"failed","errorMessage":"pool pool_T1 bootstrap provision: bootstrap stopped making progress"}'; exit 0
	fi
	[ ! -e "$STUB/pool.deleted" ] || { echo 'request failed: 404 Not Found' >&2; exit 1; }
	cat "$STUB/pool.state" 2>/dev/null || echo '{"state":"active","ready":true,"schedulable":true,"resources":{"snapshots":{"held":[{"key":"snapshot-1"}]}}}' ;;
"admin pool rm") touch "$STUB/pool.deleted" ;;
"admin provider get")
	if [ -e "$STUB/provider.deleted" ]; then cat "$STUB/provider.gone" >&2 2>/dev/null || echo 'request failed: 404 Not Found' >&2; exit 1; fi
	echo '{}' ;;
"admin provider rm") touch "$STUB/provider.deleted" ;;
"admin box purge") ;;
"admin box update")
	[ ! -e "$STUB/box-update-refused" ] || { echo 'request failed: 403 Forbidden' >&2; exit 1; } ;;
run*)
	if [ -e "$STUB/created-first" ]; then
		echo '{"id":"sbx_T2"}'
		exit 0
	fi
	if [ -e "$STUB/no-default-harness" ] && ! printf '%s\n' "$@" | grep -qx -- -H; then
		echo 'request failed: 409 Conflict: no harness given and this project has no default; pass --harness, or set one with ` + "`discobox admin harness set-default <harness>`" + `' >&2; exit 1
	fi
	touch "$STUB/created-first"
	prompt=$(printf '%s\n' "$@" | awk 'match($0, /string [0-9a-f]+ written/) { print substr($0, RSTART+7, RLENGTH-15) }')
	# Reversed in awk: rev is util-linux's, which neither the devShell nor a
	# discobox provides.
	printf '%s' "$prompt" | awk '{ for (i = length($0); i > 0; i--) printf "%s", substr($0, i, 1) }' >"$STUB/answer"
	# The CLI's progress goes to stderr, beside the JSON.
	echo "Creating discobox... syncing SSH config" >&2
	echo '{"id":"sbx_T1"}' ;;
wait*) echo finished ;;
capture*) echo "> reply"; cat "$STUB/answer"; echo ;;
attach*) echo "the screen" ;;
exec*)
	# The listener the closed-proxy step starts prints the ports it bound.
	case "$*" in
	*"claude -p"*)
		prompt=$(printf '%s\n' "$@" | grep -o 'string [0-9a-f]* written' | awk '{print $2}')
		printf '%s' "$prompt" | awk '{for (i = length($0); i > 0; i--) printf "%s", substr($0, i, 1); print ""}' ;;
	*/run/discobox/secrets/secrets.json*) echo true ;;
	*discobox-live-run-listener*) cat "$STUB/listener" 2>/dev/null || printf 'busy \nlistening 8080 8000 3000 5000 5173\n' ;;
	*)
		# $STUB/probes-before, while it is there, is what the probes see
		# once, before the discobox's narrowed level reaches its proxy.
		if [ -e "$STUB/probes-before" ]; then cat "$STUB/probes-before"; rm "$STUB/probes-before"; else cat "$STUB/probes"; fi ;;
	esac ;;
*) echo "stub discobox: unexpected call: $*" >&2; exit 3 ;;
esac
`

// stubDiscoVM lists what $STUB/left-<scope> names in a scope, and removes
// it on rm.
const stubDiscoVM = `#!/usr/bin/env bash
set -eu
printf '%s\n' "disco-vm $*" >>"$STUB/calls"
[ "$2" != boxd ] || [[ "${BOXD_REGION:-}" = fra || "${BOXD_REGION:-}" = nyc ]] || { echo 'disco-vm: boxd: configuration: region is required' >&2; exit 1; }
[ "$1 $3" = "--driver --pool" ] || { echo "stub disco-vm: no driver and scope: $*" >&2; exit 3; }
scope=$4; shift 4
case "$1 ${2:-}" in
"ls -a")
	[ ! -e "$STUB/ls-fails-$scope" ] || { echo 'disco-vm: boxd: exchange API key: 401 Unauthorized: invalid API key' >&2; exit 1; }
	echo 'ID  NAME  FROM  OS  STATE  CREATED'
	if [ "$BOXD_REGION" != "$(cat "$STUB/provider-region")" ]; then exit 0; fi
	if [ "$scope" = pool-t1 ] && [ ! -e "$STUB/pool.deleted" ]; then
		echo "abc sbx-t1 - linux running 1m"
	fi
	if [ -e "$STUB/left-$scope" ]; then
		while read -r n; do echo "abc  $n  -  linux  running  1m"; done <"$STUB/left-$scope"
	fi ;;
"exec pool-t1") echo true ;;
"cache ls") echo '[]' ;;
"key ls")
	# boxd refuses key management, the listing included, to an API key,
	# which is all the run holds (disco-vm docs/drivers/boxd.md).
	cat "$STUB/keys-$scope" 2>/dev/null || { echo 'boxd: list keys: rpc error: code = PermissionDenied desc = this operation requires an interactive session' >&2; exit 1; } ;;
"key revoke") echo '[]' >"$STUB/keys-$scope" ;;
"rm --force") grep -vx "$3" "$STUB/left-$scope" >"$STUB/left.tmp" || true; mv "$STUB/left.tmp" "$STUB/left-$scope" ;;
"url "*)
	[ ! -e "$STUB/url-err" ] || { cat "$STUB/url-err" >&2; exit 1; }
	# A sandbox machine's ingress URL is off, and disco-vm url refuses it
	# (its Engine.URL), unless $STUB/url says the driver reports one: on
	# boxd, its default proxy's domain.
	[ -e "$STUB/url" ] || { echo "disco-vm: instance $2 has no ingress URL: the boxd driver gives none to an instance whose ingress URL is asked off: machine: not supported by this driver" >&2; exit 1; }
	echo "https://dvm-$scope--$2.boxd.sh" ;;
*) echo "stub disco-vm: unexpected call: $*" >&2; exit 3 ;;
esac
`

// stubCurl is a probe of the machine's ingress URL, from outside: it prints
// $STUB/ingress-code as the status, $STUB/ingress-body as the body, and
// writes $STUB/ingress-headers, as an HTTP response's header lines, where -D
// asks for them. With
// none it answers 502, as boxd does at a default proxy locked to port 0
// (disco-vm docs/drivers/boxd.md). On boxd the URL disco-vm reports and the
// default proxy are one host, so both probes get the same answer. The body
// is not boxd's, which no run has recorded yet; the script reads it only for
// what the guest would send.
const stubCurl = `#!/usr/bin/env bash
printf '%s\n' "curl $*" >>"$STUB/calls"
out=
headers=
while [ "$#" -gt 0 ]; do case $1 in -o) out=$2; shift 2 ;; -D) headers=$2; shift 2 ;; *) shift ;; esac; done
cat "$STUB/ingress-body" >"$out" 2>/dev/null || : >"$out"
[ -z "$headers" ] || cat "$STUB/ingress-headers" >"$headers" 2>/dev/null || : >"$headers"
code=$(cat "$STUB/ingress-code" 2>/dev/null || echo 502)
printf '%s' "$code"
[ "$code" != 000 ] || exit 7
`

// passingProbes are what the probes print where the fence and the proxy
// hold: the proxy answers a refused plain-HTTP request 403 "blocked by
// proxy", and around it no connection is made (time_connect 0).
//
//nolint:gosec // G101: probe answers, not a credential.
const passingProbes = `proxy-env http://discobox-pool-proxy:3128
allowed-proxy 200 200 0 0.012 <!doctype html>
unlisted-proxy 403 000 0 0.001 blocked by proxy
ip-proxy 403 000 0 0.001 blocked by proxy
fence-direct 000 000 28 0.000000
allowed-direct 000 000 6 0.000000
unlisted-direct 000 000 6 0.000000
ip-direct 000 000 28 0.000000
`

type run struct {
	stub   string
	out    string
	calls  []string
	exit   int
	report string
}

// liveRun runs the script, live, against the stubs, with setup adjusting
// the stubs' state first.
func liveRun(t *testing.T, setup func(stub string)) run {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script is bash")
	}
	stub := t.TempDir()
	bin := filepath.Join(stub, "bin")
	write(t, filepath.Join(bin, "discobox"), stubDiscobox, 0o755)
	write(t, filepath.Join(bin, "disco-vm"), stubDiscoVM, 0o755)
	write(t, filepath.Join(bin, "curl"), stubCurl, 0o755)
	write(t, filepath.Join(stub, "probes"), passingProbes, 0o644)
	if setup != nil {
		setup(stub)
	}
	evidence := filepath.Join(stub, "evidence")
	report := filepath.Join(evidence, "report.md")
	args := []string{script(t), "--driver", "boxd", "--egress-narrowed", "yes", "--evidence", evidence, "--attach-for", "0s", "--egress-timeout", "0s", "--server", "unix:///tmp/live-run-test.sock"}
	if extra, err := os.ReadFile(filepath.Join(stub, "args")); err == nil {
		args = append(args, strings.Fields(string(extra))...)
	}
	cmd := exec.CommandContext(t.Context(), "bash", args...) //nolint:gosec // G204: the repository's own script, with the test's own arguments
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB="+stub, "BOXD_API_KEY="+key, "DISCOBOX=", "DISCO_VM=")
	out, err := cmd.CombinedOutput()
	r := run{stub: stub, out: string(out)}
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		r.exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(stub, "calls"))
	r.calls = strings.Split(strings.TrimSpace(string(calls)), "\n")
	rep, _ := os.ReadFile(report)
	r.report = string(rep)
	return r
}

func TestLiveRunPassesAndLeavesNothing(t *testing.T) {
	r := liveRun(t, nil)
	if r.exit != 0 {
		t.Fatalf("exit %d, want 0:\n%s", r.exit, r.out)
	}
	if !strings.Contains(r.report, "Every check passed, and nothing is left.") {
		t.Fatalf("report does not say it passed:\n%s", r.report)
	}
	for _, step := range []string{"provider", "pool", "pool-ready", "discobox", "harness", "attach", "provision",
		"egress-level", "egress-allowed", "egress-unlisted-proxy", "egress-ip-proxy", "egress-allowed-direct", "egress-unlisted-direct", "egress-ip-direct",
		"ingress-url", "closed-proxy", "region", "sentinels", "pool-credential", "snapshot", "snapshot-start", "snapshot-sentinels", "channel", "cleanup-second-discobox", "cleanup-discobox", "cleanup-pool", "cleanup-provider", "left-pool-scope", "left-provider-scope"} {
		if !strings.Contains(r.report, "| PASS | "+step+" |") {
			t.Errorf("report has no passing %s row:\n%s", step, r.report)
		}
	}
	// The discobox is restricted to the allowed host, and the egress probes
	// run against the hosts the run names.
	assertCall(t, r, "discobox --server unix:///tmp/live-run-test.sock admin box update sbx_T1 --egress-deny-by-default --egress-allow example.com")
	// The cleanup's order: the discobox, the pool (its grant of the key goes
	// with it), the provider, then what the provider holds, in each scope.
	order := []string{"admin box purge sbx_T2", "admin box purge sbx_T1", "admin pool rm pool_T1", "admin provider rm prov_T1",
		"--pool pool-t1 ls -a", "--pool prov-t1 ls -a"}
	last := -1
	for _, want := range order {
		i := indexOf(r.calls[last+1:], want)
		if i < 0 {
			t.Fatalf("%q is not called in order %v:\n%s", want, order, strings.Join(r.calls, "\n"))
		}
		last += i + 1
	}
}

func TestLiveRunChecksNonDefaultRegion(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "args"), "--region nyc", 0o644)
	})
	if r.exit != 0 || !strings.Contains(r.report, "| PASS | region | sbx-t1 is listed in nyc and absent from fra") {
		t.Fatalf("non-default region run failed:\n%s", r.report)
	}
	assertCall(t, r, "https://dvm-pool-t1--sbx-t1.nyc.boxd.sh")
}

func TestLiveRunRequiresHarnessCredential(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "harness-binding"), `{"boundSecrets":[]}`+"\n", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "Claude Code has no bound Anthropic credential") {
		t.Fatalf("run did not reject an absent credential:\n%s", r.report)
	}
	if indexOf(r.calls, "admin provider create") >= 0 {
		t.Fatal("the provider was created before preflight passed")
	}
}

func TestLiveRunRejectsUnrestrictedHarnessCredential(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "harness-binding"), `{"boundSecrets":[{"use":true,"envVar":"ANTHROPIC_API_KEY","routes":[[{"hosts":["*"]}]]}]}`+"\n", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "no bound Anthropic credential limited to the harness's hosts") {
		t.Fatalf("run did not reject an unrestricted credential:\n%s", r.report)
	}
}

func TestLiveRunRejectsSecondBoxFromBase(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "second-box"), `{"runtime":{"state":"running","startedFrom":{"from":"base"}}}`+"\n", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | snapshot-start | sbx_T2 started from no snapshot, expected snapshot-1") {
		t.Fatalf("run did not reject the second box's origin:\n%s", r.report)
	}
	assertCall(t, r, "admin box purge sbx_T2")
}

// The run narrows the discobox's own egress level, never the pool's: a
// pool's level holds the box's provisioning and the snapshot build too
// (store.ProvisionEgress leaves out only the sandbox's own level), so a pool
// restricted to the allowed host alone cannot provision anything. The box is
// narrowed before its egress is probed.
func TestLiveRunNarrowsTheDiscoboxNotThePool(t *testing.T) {
	r := liveRun(t, nil)
	for _, c := range r.calls {
		if strings.Contains(c, "admin pool update") && strings.Contains(c, "--egress") {
			t.Errorf("the pool's egress level is restricted, which holds provisioning to it: %q", c)
		}
	}
	narrow := indexOf(r.calls, "admin box update sbx_T1 --egress-deny-by-default --egress-allow example.com")
	if narrow < 0 {
		t.Fatalf("the discobox's own egress level is never restricted:\n%s", strings.Join(r.calls, "\n"))
	}
	if probe := indexOf(r.calls, "exec sbx_T1 -- sh -lc"); probe >= 0 && probe < narrow {
		t.Fatalf("egress is probed before the discobox is narrowed:\n%s", strings.Join(r.calls, "\n"))
	}
}

// A box started from the base provisions under its own level as well, and
// some features after readiness, so the discobox is narrowed only once its
// provisioning is done; a feature that failed is reported.
func TestLiveRunNarrowsOnceProvisioned(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "provision"),
			`{"id":"sbx_T1","runtime":{"agentStatus":{"provision":{"state":"failed","features":{"core":{"state":"ready"},"go":{"state":"failed","error":"step go failed: exit status 56"}}}}}}`, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with a feature that failed to provision:\n%s", r.out)
	}
	if !strings.Contains(r.report, "| FAIL | provision | failed: go (step go failed: exit status 56) |") {
		t.Fatalf("report does not name the failed feature:\n%s", r.report)
	}
	read := indexOf(r.calls, "admin box get sbx_T1")
	narrow := indexOf(r.calls, "admin box update sbx_T1")
	if read < 0 || narrow < 0 || narrow < read {
		t.Fatalf("the discobox is not narrowed after its provisioning is read:\n%s", strings.Join(r.calls, "\n"))
	}
}

// Provisioning failed on readiness, with no feature failed: the document's
// own error is what says why.
func TestLiveRunReportsAFailedReadiness(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "provision"),
			`{"id":"sbx_T1","runtime":{"agentStatus":{"provision":{"state":"failed","error":"harness requires git, which is not ready","features":{"core":{"state":"ready"}}}}}}`, 0o644)
	})
	if !strings.Contains(r.report, "| FAIL | provision | failed: harness requires git, which is not ready |") {
		t.Fatalf("report does not carry readiness's error:\n%s", r.report)
	}
}

// The discobox converges onto its narrowed level while running, so probes
// that still see the unlisted host answered run again, and only the answers
// after the proxy refuses it are judged.
func TestLiveRunProbesAgainUntilTheNarrowingHolds(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "args"), "--egress-timeout 5s", 0o644)
		write(t, filepath.Join(stub, "probes-before"), strings.Replace(passingProbes,
			"unlisted-proxy 403 000 0 0.001 blocked by proxy", "unlisted-proxy 200 000 0 0.010 <!doctype html>", 1), 0o644)
	})
	if r.exit != 0 {
		t.Fatalf("exit %d, want 0:\n%s", r.exit, r.out)
	}
	if !strings.Contains(r.report, "| PASS | egress-unlisted-proxy |") {
		t.Fatalf("report has no passing egress-unlisted-proxy row:\n%s", r.report)
	}
	if !strings.Contains(r.out, "the narrowed level reached the discobox's proxy after 5s") {
		t.Fatalf("the run does not say when the narrowing held:\n%s", r.out)
	}
	probes := 0
	for _, c := range r.calls {
		if strings.Contains(c, " exec sbx_T1 -- sh -lc") {
			probes++
		}
	}
	if probes != 2 {
		t.Fatalf("the probes ran %d times, want 2:\n%s", probes, strings.Join(r.calls, "\n"))
	}
}

// Probes whose proxy never refuses the unlisted host are judged once
// --egress-timeout is up, and fail; the run does not say the narrowing held.
func TestLiveRunJudgesTheProbesWhenTheNarrowingNeverHolds(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "probes"), strings.Replace(passingProbes,
			"unlisted-proxy 403 000 0 0.001 blocked by proxy", "unlisted-proxy 200 000 0 0.010 <!doctype html>", 1), 0o644)
	})
	if !strings.Contains(r.report, "| FAIL | egress-unlisted-proxy |") {
		t.Fatalf("report has no failing egress-unlisted-proxy row:\n%s", r.report)
	}
	if strings.Contains(r.out, "reached the discobox's proxy") {
		t.Fatalf("the run says the narrowing held when it never did:\n%s", r.out)
	}
}

// A discobox whose own level cannot be narrowed fails the run, and its
// egress is not probed: open, the probes would prove nothing.
func TestLiveRunFailsWhenTheDiscoboxCannotBeNarrowed(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "box-update-refused"), "", 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with the discobox's egress open:\n%s", r.out)
	}
	if !strings.Contains(r.report, "| FAIL | egress-level | could not restrict the discobox's egress to example.com") {
		t.Fatalf("report does not say the narrowing failed:\n%s", r.report)
	}
	if strings.Contains(r.report, "| PASS | egress-") {
		t.Fatalf("egress was judged on an open discobox:\n%s", r.report)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " exec sbx_T1 -- sh -lc") {
			t.Fatalf("egress was probed on an open discobox: %q", c)
		}
	}
}

// The key reaches the provider's config and nothing else: no argv, no
// output, no report.
func TestLiveRunKeyOnlyInProviderConfig(t *testing.T) {
	r := liveRun(t, nil)
	config, err := os.ReadFile(filepath.Join(r.stub, "provider.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"apiKey": "` + key + `"`; !strings.Contains(string(config), want) || !strings.Contains(string(config), `"driver": {`) {
		t.Fatalf("provider config %s does not hold the key", config)
	}
	if !strings.Contains(string(config), `"region": "fra"`) {
		t.Fatalf("provider config %s names no region, which boxd requires", config)
	}
	if strings.Contains(string(config), "deliverProviderKey") {
		t.Fatalf("provider config %s puts the key on the pool's machine", config)
	}
	for _, where := range []string{strings.Join(r.calls, "\n"), r.out, r.report} {
		if strings.Contains(where, key) {
			t.Fatalf("the key leaked:\n%s", where)
		}
	}
}

func TestLiveRunReportsAndRemovesLeftovers(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "left-pool-t1"), "sbx-t1\n", 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with a machine left:\n%s", r.out)
	}
	if !strings.Contains(r.report, "| FAIL | left-pool-scope | left in scope pool-t1: machines [sbx-t1]") {
		t.Fatalf("report does not name what was left:\n%s", r.report)
	}
	if !strings.Contains(r.report, "| PASS | left-pool-scope-removed |") || !strings.Contains(r.report, "Something was left under the run's scopes") {
		t.Fatalf("report does not say the leftover was found and removed:\n%s", r.report)
	}
	assertCall(t, r, "disco-vm --driver boxd --pool pool-t1 rm --force sbx-t1")
}

// A pool that fails stops the run, and the cleanup still deletes what was
// made.
func TestLiveRunCleansUpAfterAFailedPool(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "pool.state"), `{"state":"failed","errorMessage":"boxd: quota"}`, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with a failed pool:\n%s", r.out)
	}
	if !strings.Contains(r.report, "| FAIL | pool-ready | failed: boxd: quota |") {
		t.Fatalf("report does not carry the pool's error:\n%s", r.report)
	}
	for _, want := range []string{"admin pool rm pool_T1", "admin provider rm prov_T1", "--pool pool-t1 ls -a", "--pool prov-t1 ls -a"} {
		assertCall(t, r, want)
	}
	if indexOf(r.calls, " run ") >= 0 {
		t.Fatalf("a discobox was made on a failed pool:\n%s", strings.Join(r.calls, "\n"))
	}
}

func TestLiveRunEgressFailures(t *testing.T) {
	r := liveRun(t, func(stub string) {
		probes := strings.NewReplacer(
			// The proxy let an unlisted host through.
			"unlisted-proxy 403 000 0 0.001 blocked by proxy", "unlisted-proxy 200 000 0 0.010 <html>",
			// A refused CONNECT looks like a closed connection, which a
			// network failure does too: not the proxy's refusal.
			"ip-proxy 403 000 0 0.001 blocked by proxy", "ip-proxy 000 000 56 0.000000",
			// An origin answered around the proxy, over TLS.
			"ip-direct 000 000 28 0.000000", "ip-direct 200 000 0 0.031 <html>").Replace(passingProbes)
		write(t, filepath.Join(stub, "probes"), probes, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with open egress:\n%s", r.out)
	}
	for _, want := range []string{"| FAIL | egress-unlisted-proxy |", "| FAIL | egress-ip-proxy |",
		"| FAIL | egress-ip-direct | 1.1.1.1 answered around the proxy: HTTP 200, curl 0, connected after 0.031s: <html>", "| PASS | egress-allowed |", "| PASS | egress-unlisted-direct |"} {
		if !strings.Contains(r.report, want) {
			t.Errorf("report has no %q:\n%s", want, r.report)
		}
	}
}

// boxd's fence accepts the TCP connection around the proxy and refuses it
// there: TLS closes at the ClientHello (curl 35), whatever the host, the
// allowed one included. Nothing reached an origin, so the probes pass.
func TestLiveRunDirectEgressRefusedByAcceptingFence(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "probes"), boxdFenceProbes, 0o644)
	})
	for _, want := range []string{"| PASS | egress-allowed-direct |", "| PASS | egress-unlisted-direct |", "| PASS | egress-ip-direct |"} {
		if !strings.Contains(r.report, want) {
			t.Errorf("report has no %q:\n%s", want, r.report)
		}
	}
}

// boxdFenceProbes are boxd's answers around the proxy: its fence accepts
// every connection and closes TLS at the ClientHello. The real hosts'
// answers are #1050's B6 re-run's. No run has probed 192.0.2.1 yet: its
// answer here is assumed to be the same refusal, which the next live run on
// boxd confirms (a CHECK row if it is not).
var boxdFenceProbes = strings.NewReplacer(
	"fence-direct 000 000 28 0.000000", "fence-direct 000 000 35 0.000310",
	"allowed-direct 000 000 6 0.000000", "allowed-direct 000 000 35 0.023210",
	"unlisted-direct 000 000 6 0.000000", "unlisted-direct 000 000 35 0.073857",
	"ip-direct 000 000 28 0.000000", "ip-direct 000 000 35 0.000302").Replace(passingProbes)

// Behind a fence that accepts, what may be an origin's answer still fails:
// a certificate presented, or a reset after the connection.
func TestLiveRunDirectEgressOriginBehindAcceptingFence(t *testing.T) {
	r := liveRun(t, func(stub string) {
		probes := strings.NewReplacer(
			"unlisted-direct 000 000 35 0.073857", "unlisted-direct 000 000 60 0.073857",
			"ip-direct 000 000 35 0.000302", "ip-direct 000 000 56 0.000302").Replace(boxdFenceProbes)
		write(t, filepath.Join(stub, "probes"), probes, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with an origin reached:\n%s", r.out)
	}
	for _, want := range []string{"| PASS | egress-allowed-direct |",
		"| FAIL | egress-unlisted-direct | example.net around the proxy: HTTP 000, curl 60, connected after 0.073857s",
		"| FAIL | egress-ip-direct | 1.1.1.1 around the proxy: HTTP 000, curl 56, connected after 0.000302s"} {
		if !strings.Contains(r.report, want) {
			t.Errorf("report has no %q:\n%s", want, r.report)
		}
	}
}

// A fence that forwards connections to any host, and an origin that closes
// TLS, look like boxd's refusal at a real host. Only 192.0.2.1, where no
// server answers, tells them apart: when it does not connect, curl 35 at a
// real host passes nothing and is for a person to read.
func TestLiveRunDirectEgressRefusalWithoutFenceBaseline(t *testing.T) {
	r := liveRun(t, func(stub string) {
		probes := strings.NewReplacer("fence-direct 000 000 35 0.000310", "fence-direct 000 000 28 0.000000").Replace(boxdFenceProbes)
		write(t, filepath.Join(stub, "probes"), probes, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with the fence's refusal unproven:\n%s", r.out)
	}
	for _, step := range []string{"egress-allowed-direct", "egress-unlisted-direct", "egress-ip-direct"} {
		if want := "| CHECK | " + step + " |"; !strings.Contains(r.report, want) {
			t.Errorf("report has no %q:\n%s", want, r.report)
		}
	}
}

// A discobox with no curl proves nothing about its egress.
func TestLiveRunEgressWithoutCurlFails(t *testing.T) {
	r := liveRun(t, func(stub string) { write(t, filepath.Join(stub, "probes"), "no-curl\n", 0o644) })
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | egress | the discobox has no curl") {
		t.Fatalf("exit %d, want an egress failure:\n%s", r.exit, r.report)
	}
}

func TestLiveRunIngressURL(t *testing.T) {
	for _, tc := range []struct {
		name, code, body, want string
	}{
		// The request reached the guest's own TLS listener.
		{"guest listener", "400", "Client sent an HTTP request to an HTTPS server.\n", "| FAIL | ingress-url | https://dvm-pool-t1--sbx-t1.boxd.sh reaches the guest's listener"},
		{"guest answers", "200", "ok\n", "| FAIL | ingress-url | https://dvm-pool-t1--sbx-t1.boxd.sh answers 200"},
		// An answer the script cannot place is for a person to read.
		{"provider answers", "502", "bad gateway\n", "| CHECK | ingress-url | https://dvm-pool-t1--sbx-t1.boxd.sh answers HTTP 502"},
		// Nothing reached is nothing known: this box's own egress, DNS or a
		// timeout may have stopped the probe.
		{"not reached", "000", "", "| CHECK | ingress-url | https://dvm-pool-t1--sbx-t1.boxd.sh was not reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := liveRun(t, func(stub string) {
				write(t, filepath.Join(stub, "url"), "", 0o644)
				write(t, filepath.Join(stub, "ingress-code"), tc.code, 0o644)
				write(t, filepath.Join(stub, "ingress-body"), tc.body, 0o644)
			})
			if r.exit == 0 || !strings.Contains(r.report, tc.want) {
				t.Fatalf("exit %d, want %q:\n%s", r.exit, tc.want, r.report)
			}
		})
	}
}

// A sandbox machine's ingress URL is off, so disco-vm reports none, but boxd
// keeps its default proxy, locked to port 0, at the machine's name in its
// zone. The run probes that host itself, whatever disco-vm url said, while
// the guest listens where an open proxy would forward: boxd's 502 then
// passes, an answer from the guest fails, and anything else is for a person
// to read.
func TestLiveRunProbesClosedProxy(t *testing.T) {
	const host = "https://dvm-pool-t1--sbx-t1.boxd.sh"
	for _, tc := range []struct {
		name, code, body, listener, headers, want string
	}{
		{"boxd refuses", "502", "", "", "", "| PASS | closed-proxy | " + host + " answers 502 while the guest listened on 8080 8000 3000 5000 5173: forwarded nowhere, closed"},
		// An open proxy forwards to the guest's listener.
		{"listener answers", "200", "discobox-live-run-listener\n", "", "", "| FAIL | closed-proxy | " + host + " forwards to the guest: HTTP 200, its listener's marker came back"},
		{"guest answers", "200", "ok\n", "", "", "| FAIL | closed-proxy | " + host + " answers 200"},
		{"guest listener", "400", "Client sent an HTTP request to an HTTPS server.\n", "", "", "| FAIL | closed-proxy | " + host + " reaches the guest's listener"},
		// With nothing listening on 8000, an open proxy answers 502 too.
		{"no listener", "502", "", "busy 8000\nlistening 3000\n", "", "| CHECK | closed-proxy | " + host + " answers 502, but the guest did not hold 8000"},
		{"no python", "502", "", "no-python3\n", "", "| CHECK | closed-proxy | " + host + " answers 502, but the guest did not hold 8000"},
		// Something already on a port ahead of 8000 is where an open proxy
		// forwards, and it, or boxd for it, may answer 502.
		{"busy ahead", "502", "", "busy 443\nlistening 8080 8000 3000 5000 5173\n", "", "| CHECK | closed-proxy | " + host + " answers 502, but the guest did not hold 8000 with nothing else ahead of it (listening on: 8080 8000 3000 5000 5173; already held: 443"},
		// What listens could not be read, so a port ahead of 8000 may be held.
		{"unknown", "502", "", "busy unknown\nlistening 8080 8000 3000 5000 5173\n", "", "| CHECK | closed-proxy | " + host + " answers 502, but the guest did not hold 8000 with nothing else ahead of it (listening on: 8080 8000 3000 5000 5173; already held: unknown"},
		// Without root, 80 and 443 are unbound; with nothing listening there
		// (on any address), they are no one's.
		{"no root", "502", "", "busy \nlistening 8080 8000 3000 5000 5173\n", "", "| PASS | closed-proxy |"},
		{"unplaced", "404", "not found\n", "", "", "| CHECK | closed-proxy | " + host + " answers HTTP 404"},
		{"not reached", "000", "", "", "", "| CHECK | closed-proxy | " + host + " was not reached"},
		// boxd's bot protection answers a challenge in place of the closed
		// proxy's 502, which a disco-vm pin from before it turned that off
		// there leaves on: named, so a stale pin reads as one.
		{"bot challenge", "503", "<!DOCTYPE html><title>Just a moment</title>\n", "", "HTTP/2 503\r\nx-boxd-proxy: challenge\r\nserver: boxd\r\n\r\n", "| CHECK | closed-proxy | " + host + " answers boxd's bot-protection challenge (HTTP 503, x-boxd-proxy: challenge)"},
		// Any other 503 is for a person to read.
		{"unavailable", "503", "busy\n", "", "HTTP/2 503\r\nserver: boxd\r\n\r\n", "| CHECK | closed-proxy | " + host + " answers HTTP 503 (server 'boxd'): busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := liveRun(t, func(stub string) {
				write(t, filepath.Join(stub, "ingress-code"), tc.code, 0o644)
				write(t, filepath.Join(stub, "ingress-body"), tc.body, 0o644)
				if tc.listener != "" {
					write(t, filepath.Join(stub, "listener"), tc.listener, 0o644)
				}
				if tc.headers != "" {
					write(t, filepath.Join(stub, "ingress-headers"), tc.headers, 0o644)
				}
			})
			if !strings.Contains(r.report, tc.want) {
				t.Fatalf("exit %d, want %q:\n%s", r.exit, tc.want, r.report)
			}
			if pass := strings.Contains(tc.want, "| PASS |"); pass != (r.exit == 0) {
				t.Fatalf("exit %d with %q:\n%s", r.exit, tc.want, r.report)
			}
			// disco-vm url refused, as it does for a machine with its
			// ingress URL off, and that is no failure of its own.
			if !strings.Contains(r.report, "| PASS | ingress-url | the driver reports no URL for sbx-t1") {
				t.Fatalf("ingress-url does not pass on disco-vm's refusal:\n%s", r.report)
			}
			// The probe goes from outside, while the guest listens.
			if i, j := indexOf(r.calls, "discobox-live-run-listener"), indexOf(r.calls, "curl -sS -o "+filepath.Join(r.stub, "evidence", "closed-proxy.body")); i < 0 || j < 0 || i > j {
				t.Fatalf("the proxy is not probed after the guest's listener starts:\n%s", strings.Join(r.calls, "\n"))
			}
			if tc.code != "000" {
				if _, err := os.Stat(filepath.Join(r.stub, "evidence", "closed-proxy.headers")); err != nil {
					t.Fatalf("the proxy's answer is not kept: %v", err)
				}
			}
		})
	}
}

// A URL the driver does report is its default proxy, open: that it answers
// fails both steps.
func TestLiveRunReportedURLIsTheProxy(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "url"), "", 0o644)
		write(t, filepath.Join(stub, "ingress-code"), "200", 0o644)
		write(t, filepath.Join(stub, "ingress-body"), "discobox-live-run-listener\n", 0o644)
	})
	for _, want := range []string{"| FAIL | ingress-url | https://dvm-pool-t1--sbx-t1.boxd.sh answers 200", "| FAIL | closed-proxy | https://dvm-pool-t1--sbx-t1.boxd.sh forwards to the guest"} {
		if !strings.Contains(r.report, want) {
			t.Errorf("report has no %q:\n%s", want, r.report)
		}
	}
}

// disco-vm failing for any other reason is not a machine with no URL.
func TestLiveRunURLUnread(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "url-err"), "disco-vm: boxd: exchange API key: 401 Unauthorized\n", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| CHECK | ingress-url | the driver gives sbx-t1 no URL it could read: disco-vm: boxd: exchange API key: 401") {
		t.Fatalf("exit %d:\n%s", r.exit, r.report)
	}
}

// What the guest wrote reaches the report with no control characters.
func TestLiveRunEscapesWhatTheGuestWrote(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "url"), "", 0o644)
		write(t, filepath.Join(stub, "ingress-code"), "502", 0o644)
		// ESC sequences, a C1 CSI sent as UTF-8, and a bidi override.
		write(t, filepath.Join(stub, "ingress-body"), "\x1b[2K\x1b[1A\xc2\x9b2Kall\u202e clear\r\n", 0o644)
	})
	for _, bad := range []string{"\x1b", "\xc2\x9b", "\u202e", "\r"} {
		if strings.Contains(r.out, bad) || strings.Contains(r.report, bad) {
			t.Fatalf("%q from the guest was printed:\n%q", bad, r.report)
		}
	}
	if !strings.Contains(r.report, "2Kall") || !strings.Contains(r.report, "clear") {
		t.Fatalf("the guest's text is not in the report:\n%s", r.report)
	}
}

// A delete is confirmed by the server saying the resource is not there; any
// other failure to read it is not that.
func TestLiveRunGoneNeedsNotFound(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "provider.gone"), "request failed: 503 Service Unavailable\n", 0o644)
		write(t, filepath.Join(stub, "args"), "--pool-timeout 0s", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | cleanup-provider | provider prov_T1 is not gone after 0s: request failed: 503") {
		t.Fatalf("exit %d, want the provider's delete unconfirmed:\n%s", r.exit, r.report)
	}
	if !strings.Contains(r.report, "| PASS | cleanup-pool | pool pool_T1 is gone") {
		t.Fatalf("the pool's 404 did not confirm its delete:\n%s", r.report)
	}
}

// A scope disco-vm cannot list is unknown, not left: the report says which.
func TestLiveRunUnlistedScopeIsNotCalledLeft(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "ls-fails-prov-t1"), "", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | left-provider-scope-listed | disco-vm could not list scope prov-t1's machines (disco-vm: boxd: exchange API key: 401") {
		t.Fatalf("exit %d, want the listing failure:\n%s", r.exit, r.report)
	}
	if strings.Contains(r.report, "Something was left") || !strings.Contains(r.report, "so what is left there is not known") {
		t.Fatalf("an unlisted scope was reported as something left:\n%s", r.report)
	}
}

// boxd refuses an API key the listing of keys; the key could not issue any
// either, so nothing was issued to leave, and the machine sweep still runs.
func TestLiveRunKeyListingRefusedToAnAPIKey(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "left-pool-t1"), "sbx-t1\n", 0o644)
	})
	if strings.Contains(r.report, "-listed |") {
		t.Fatalf("a refused key listing was reported as a failed listing:\n%s", r.report)
	}
	if !strings.Contains(r.report, "| FAIL | left-pool-scope | left in scope pool-t1: machines [sbx-t1] caches [] keys []") ||
		!strings.Contains(r.report, "| PASS | left-provider-scope |") {
		t.Fatalf("the sweep did not run past the key listing:\n%s", r.report)
	}
	assertCall(t, r, "disco-vm --driver boxd --pool pool-t1 rm --force sbx-t1")
}

// Inside a discobox the CLI's default server is the gate to the outer
// server, which must never get the key.
func TestLiveRunRefusesTheGate(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "args"), "--server https://api.discobox.internal", 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | preflight | the server is https://api.discobox.internal") {
		t.Fatalf("exit %d, want the gate refused:\n%s", r.exit, r.report)
	}
	if indexOf(r.calls, "discobox ") >= 0 {
		t.Fatalf("the CLI was called with the gate as its server:\n%s", strings.Join(r.calls, "\n"))
	}
}

// Keys issued for the pool are leftovers too, and are revoked.
func TestLiveRunRevokesLeftoverKeys(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "keys-prov-t1"), `[{"id":"k1","name":"pool-t1-abc"},{"id":"k2","name":"other-pool"}]`, 0o644)
	})
	if r.exit == 0 || !strings.Contains(r.report, "| FAIL | left-provider-scope | left in scope prov-t1: machines [] caches [] keys [k1]") {
		t.Fatalf("exit %d, want the pool's key reported:\n%s", r.exit, r.report)
	}
	assertCall(t, r, "disco-vm --driver boxd --pool prov-t1 key revoke k1")
	if indexOf(r.calls, "revoke k2") >= 0 {
		t.Fatalf("a key not issued for the pool was revoked")
	}
}

// The evidence is kept before the cleanup deletes what it is about.
func TestLiveRunKeepsEvidenceFirst(t *testing.T) {
	r := liveRun(t, nil)
	for _, f := range []string{"discobox.json", "screen.txt", "pool.json", "pool.log", "provider.json", "egress.txt", "attach.raw", "report.md"} {
		if _, err := os.Stat(filepath.Join(r.stub, "evidence", f)); err != nil {
			t.Errorf("no evidence %s: %v", f, err)
		}
	}
	if i, j := indexOf(r.calls, "admin pool logs pool_T1"), indexOf(r.calls, "admin box purge"); i < 0 || i > j {
		t.Fatalf("the pool's log is not read before the purge:\n%s", strings.Join(r.calls, "\n"))
	}
	if !strings.Contains(r.report, "This box's egress narrowed before the key came in: yes") {
		t.Fatalf("the report does not say whether egress was narrowed:\n%s", r.report)
	}
}

// The dry run calls nothing and reads no key, and still goes through every
// step on its stand-in answers.
func TestDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is bash")
	}
	empty := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", script(t), "--driver", "boxd", "--dry-run", "--evidence", filepath.Join(empty, "evidence"), "--server", "unix:///tmp/live-run-test.sock") //nolint:gosec // G204: the repository's own script
	// Nothing on PATH but what reads answers: a call out would fail.
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not on PATH")
	}
	bin := filepath.Join(empty, "bin")
	for _, tool := range []string{"jq", "awk", "od", "tr", "date", "mktemp", "sed", "rm", "cat", "head", "wc", "grep", "tee", "dirname", "git", "mkdir", "tail", "sleep", "seq", "cp"} {
		path := jq
		if tool != "jq" {
			if path, err = exec.LookPath(tool); err != nil {
				continue
			}
		}
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	cmd.Env = append(os.Environ(), "PATH="+bin, "BOXD_API_KEY="+key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	s := string(out)
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, "command not found") || strings.Contains(line, "listen.out: No such file or directory") {
			t.Errorf("dry run shell error: %s", line)
		}
	}
	listener, err := os.ReadFile(filepath.Join(empty, "evidence", "closed-proxy.listener"))
	if err != nil {
		t.Errorf("dry run did not save listener evidence: %v", err)
	} else if !strings.Contains(string(listener), "listening 8080 8000 3000 5000 5173\n") {
		t.Errorf("dry run listener evidence has no synthetic listener result: %q", listener)
	}
	if strings.Contains(s, key) {
		t.Fatalf("the dry run printed the key:\n%s", s)
	}
	if strings.Contains(s, "| PASS |") || strings.Contains(s, "| FAIL |") || !strings.Contains(s, "The dry run went through every step") {
		t.Fatalf("the dry run's report claims an observation, or did not finish:\n%s", s)
	}
	for _, want := range []string{
		"+ discobox --server unix:///tmp/live-run-test.sock -o json admin provider create --type boxd --name live-boxd-",
		`config: {"driver":{"apiKey":"<$BOXD_API_KEY>","region":"fra"}}`,
		"+ disco-vm --driver boxd --pool pool-dryrun ls -a",
		"| DRY | left-provider-scope |",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run output has no %q", want)
		}
	}
}

func TestRequiredFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is bash")
	}
	// A live run says whether this box's egress was narrowed.
	for _, args := range [][]string{{"--dry-run"}, {"--driver", "docker", "--dry-run"}, {"--driver", "boxd"}} {
		out, err := exec.CommandContext(t.Context(), "bash", append([]string{script(t)}, args...)...).CombinedOutput() //nolint:gosec // G204: the repository's own script
		if exitErr := (*exec.ExitError)(nil); !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("%v: %v, want exit 2:\n%s", args, err, out)
		}
	}
}

func script(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../scripts/live-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertCall(t *testing.T, r run, want string) {
	t.Helper()
	if indexOf(r.calls, want) < 0 {
		t.Fatalf("no call %q among:\n%s", want, strings.Join(r.calls, "\n"))
	}
}

func indexOf(calls []string, part string) int {
	for i, c := range calls {
		if strings.Contains(c, part) {
			return i
		}
	}
	return -1
}
