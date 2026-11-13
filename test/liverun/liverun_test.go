// Package liverun_test runs scripts/live-run.sh, the one live run's recipe,
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
for a in "$@"; do case $a in --server|--server=*|http://*|-o|json) ;; *) args+=("$a") ;; esac; done
set -- "${args[@]}"
case "$1 ${2:-} ${3:-}" in
"admin provider list") echo '[]' ;;
"admin box get") echo '{"id":"sbx_T1"}' ;;
"admin pool logs") echo "pool journal" ;;
"admin provider create")
	for a in "$@"; do case $a in @*) cp "${a#@}" "$STUB/provider.json" ;; esac; done
	echo '{"id":"prov_T1"}' ;;
"admin pool create") echo '{"id":"pool_T1"}' ;;
"admin pool update") ;;
"admin pool get")
	[ ! -e "$STUB/pool.deleted" ] || { echo 'request failed: 404 Not Found' >&2; exit 1; }
	cat "$STUB/pool.state" 2>/dev/null || echo '{"state":"active","ready":true,"schedulable":true}' ;;
"admin pool rm") touch "$STUB/pool.deleted" ;;
"admin provider get")
	if [ -e "$STUB/provider.deleted" ]; then cat "$STUB/provider.gone" >&2 2>/dev/null || echo 'request failed: 404 Not Found' >&2; exit 1; fi
	echo '{}' ;;
"admin provider rm") touch "$STUB/provider.deleted" ;;
"admin box purge") ;;
run*)
	prompt=$(printf '%s\n' "$@" | grep -o 'string [0-9a-f]* written' | awk '{ print $2 }')
	printf '%s' "$prompt" | rev >"$STUB/answer"
	# The CLI's progress goes to stderr, beside the JSON.
	echo "Creating discobox... syncing SSH config" >&2
	echo '{"id":"sbx_T1"}' ;;
wait*) echo finished ;;
capture*) echo "> reply"; cat "$STUB/answer"; echo ;;
attach*) echo "the screen" ;;
exec*) cat "$STUB/probes" ;;
*) echo "stub discobox: unexpected call: $*" >&2; exit 3 ;;
esac
`

// stubDiscoVM lists what $STUB/left-<scope> names in a scope, and removes
// it on rm.
const stubDiscoVM = `#!/usr/bin/env bash
set -eu
printf '%s\n' "disco-vm $*" >>"$STUB/calls"
[ "$1 $3" = "--driver --pool" ] || { echo "stub disco-vm: no driver and scope: $*" >&2; exit 3; }
scope=$4; shift 4
case "$1 ${2:-}" in
"ls -a")
	[ ! -e "$STUB/ls-fails-$scope" ] || { echo 'disco-vm: boxd: exchange API key: 401 Unauthorized: invalid API key' >&2; exit 1; }
	echo 'ID  NAME  FROM  OS  STATE  CREATED'
	if [ -e "$STUB/left-$scope" ]; then
		while read -r n; do echo "abc  $n  -  linux  running  1m"; done <"$STUB/left-$scope"
	fi ;;
"cache ls") echo '[]' ;;
"key ls")
	# boxd refuses key management, the listing included, to an API key,
	# which is all the run holds (disco-vm docs/drivers/boxd.md).
	cat "$STUB/keys-$scope" 2>/dev/null || { echo 'boxd: list keys: rpc error: code = PermissionDenied desc = this operation requires an interactive session' >&2; exit 1; } ;;
"key revoke") echo '[]' >"$STUB/keys-$scope" ;;
"rm --force") grep -vx "$3" "$STUB/left-$scope" >"$STUB/left.tmp" || true; mv "$STUB/left.tmp" "$STUB/left-$scope" ;;
"url "*) [ ! -e "$STUB/url" ] || echo "https://$2.zone.test" ;;
*) echo "stub disco-vm: unexpected call: $*" >&2; exit 3 ;;
esac
`

// stubCurl is the probe of the public URL, from outside: it prints
// $STUB/public-code as the status, and $STUB/public-body as the body.
const stubCurl = `#!/usr/bin/env bash
printf '%s\n' "curl $*" >>"$STUB/calls"
out=
while [ "$#" -gt 0 ]; do case $1 in -o) out=$2; shift 2 ;; *) shift ;; esac; done
cat "$STUB/public-body" >"$out" 2>/dev/null || : >"$out"
code=$(cat "$STUB/public-code" 2>/dev/null || echo 000)
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
	args := []string{script(t), "--driver", "boxd", "--egress-narrowed", "yes", "--evidence", evidence, "--attach-for", "0s", "--server", "http://server.test"}
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
	for _, step := range []string{"provider", "pool", "pool-ready", "discobox", "harness", "attach",
		"egress-allowed", "egress-unlisted-proxy", "egress-ip-proxy", "egress-unlisted-direct", "egress-ip-direct",
		"public-url", "cleanup-discobox", "cleanup-pool", "cleanup-provider", "left-pool-scope", "left-provider-scope"} {
		if !strings.Contains(r.report, "| PASS | "+step+" |") {
			t.Errorf("report has no passing %s row:\n%s", step, r.report)
		}
	}
	// The pool is restricted to the allowed host, and the egress probes run
	// against the hosts the run names.
	assertCall(t, r, "discobox --server http://server.test admin pool update pool_T1 --egress-deny-by-default --egress-allow example.com")
	// The cleanup's order: the discobox, the pool (its grant of the key goes
	// with it), the provider, then what the provider holds, in each scope.
	order := []string{"admin box purge sbx_T1", "admin pool rm pool_T1", "admin provider rm prov_T1",
		"--pool pool-t1 ls -a", "--pool prov-t1 ls -a"}
	last := -1
	for _, want := range order {
		i := indexOf(r.calls, want)
		if i < 0 || i < last {
			t.Fatalf("%q is not called in order %v:\n%s", want, order, strings.Join(r.calls, "\n"))
		}
		last = i
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
			// curl failed, but after a connection was made around the proxy.
			"ip-direct 000 000 28 0.000000", "ip-direct 000 000 35 0.031").Replace(passingProbes)
		write(t, filepath.Join(stub, "probes"), probes, 0o644)
	})
	if r.exit == 0 {
		t.Fatalf("exit 0 with open egress:\n%s", r.out)
	}
	for _, want := range []string{"| FAIL | egress-unlisted-proxy |", "| FAIL | egress-ip-proxy |",
		"| FAIL | egress-ip-direct | 1.1.1.1 around the proxy: HTTP 000, curl 35, connected after 0.031s", "| PASS | egress-allowed |", "| PASS | egress-unlisted-direct |"} {
		if !strings.Contains(r.report, want) {
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

func TestLiveRunPublicURL(t *testing.T) {
	for _, tc := range []struct {
		name, code, body, want string
	}{
		// The request reached the guest's own TLS listener.
		{"guest listener", "400", "Client sent an HTTP request to an HTTPS server.\n", "| FAIL | public-url | https://sbx-t1.zone.test reaches the guest's listener"},
		{"guest answers", "200", "ok\n", "| FAIL | public-url | https://sbx-t1.zone.test answers 200"},
		// An answer the script cannot place is for a person to read.
		{"provider answers", "502", "bad gateway\n", "| CHECK | public-url | https://sbx-t1.zone.test answers HTTP 502"},
		// Nothing reached is nothing known: this box's own egress, DNS or a
		// timeout may have stopped the probe.
		{"not reached", "000", "", "| CHECK | public-url | https://sbx-t1.zone.test was not reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := liveRun(t, func(stub string) {
				write(t, filepath.Join(stub, "url"), "", 0o644)
				write(t, filepath.Join(stub, "public-code"), tc.code, 0o644)
				write(t, filepath.Join(stub, "public-body"), tc.body, 0o644)
			})
			if r.exit == 0 || !strings.Contains(r.report, tc.want) {
				t.Fatalf("exit %d, want %q:\n%s", r.exit, tc.want, r.report)
			}
		})
	}
}

// What the guest wrote reaches the report with no control characters.
func TestLiveRunEscapesWhatTheGuestWrote(t *testing.T) {
	r := liveRun(t, func(stub string) {
		write(t, filepath.Join(stub, "url"), "", 0o644)
		write(t, filepath.Join(stub, "public-code"), "502", 0o644)
		// ESC sequences, a C1 CSI sent as UTF-8, and a bidi override.
		write(t, filepath.Join(stub, "public-body"), "\x1b[2K\x1b[1A\xc2\x9b2Kall\u202e clear\r\n", 0o644)
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
	cmd := exec.CommandContext(t.Context(), "bash", script(t), "--driver", "boxd", "--dry-run", "--evidence", filepath.Join(empty, "evidence"), "--server", "http://server.test") //nolint:gosec // G204: the repository's own script
	// Nothing on PATH but what reads answers: a call out would fail.
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not on PATH")
	}
	bin := filepath.Join(empty, "bin")
	for _, tool := range []string{"jq", "awk", "od", "tr", "date", "mktemp", "sed", "rm", "cat", "head", "wc", "grep", "tee", "dirname", "git", "mkdir", "tail"} {
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
	if strings.Contains(s, key) {
		t.Fatalf("the dry run printed the key:\n%s", s)
	}
	if strings.Contains(s, "| PASS |") || strings.Contains(s, "| FAIL |") || !strings.Contains(s, "The dry run went through every step") {
		t.Fatalf("the dry run's report claims an observation, or did not finish:\n%s", s)
	}
	for _, want := range []string{
		"+ discobox --server http://server.test -o json admin provider create --type boxd --name live-boxd-",
		`config: {"driver":{"apiKey":"<$BOXD_API_KEY>"}}`,
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
