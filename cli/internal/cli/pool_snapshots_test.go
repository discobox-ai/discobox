package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// poolWithSnapshots is poolWithResources with its report's snapshots spliced
// in: one held by its plan, one held from before plans were recorded, a
// build running and one queued, and a failed plan whose error carries an
// escape sequence from the build's output.
func poolWithSnapshots(snapshots string) *apimodel.Pool {
	raw := poolWithResources
	if snapshots != "" {
		raw = strings.Replace(raw, `"cacheBytes":44023414784,"buildBytes":9663676416}}},`, `"cacheBytes":44023414784,"buildBytes":9663676416}},"snapshots":`+snapshots+`},`, 1)
	}
	var pool apimodel.Pool
	if err := pool.UnmarshalJSON([]byte(raw)); err != nil {
		panic(err)
	}
	return &pool
}

const reportedSnapshots = `{"held":[` +
	`{"key":"3f2a91c0e4d1aaaaaaaaaaaa","builtAt":"2026-08-27T11:00:00Z","sizeBytes":3221225472,"boxes":2,` +
	`"plan":{"base":"sha256:5d1c","bundles":[{"name":"discobox-platform","digest":"sha256:aaaaaaaaaaaaaaaa"},{"name":"discobox-harness-shell","digest":"sha256:bbbbbbbbbbbbbbbb"}],"off":["docker","brew"]}},` +
	`{"key":"0000000000ffffffffff","builtAt":"2026-08-26T11:00:00Z","boxes":0}],` +
	`"building":[{"key":"9b9b9b9b9b9b9b9b","requestedAt":"2026-08-27T11:55:00Z","startedAt":"2026-08-27T11:56:00Z","plan":{"base":"sha256:5d1c"},` +
	`"box":"snap-9b9b9b9b","progress":{"state":"provisioning","features":{"core":{"state":"ready"},"docker":{"state":"provisioning","step":"install\u001b[2J"},"brew":{"state":"off"}}}},` +
	`{"key":"7c7c7c7c7c7c7c7c","requestedAt":"2026-08-27T11:58:00Z","plan":{"base":"sha256:5d1c"}}],` +
	`"failed":[{"key":"e1e1e1e1e1e1e1e1","failedAt":"2026-08-27T11:30:00Z","retryAfter":"2026-08-27T12:30:00Z","error":"its provisioning failed (status 2): \u001b[31mfeature go failed"}]}`

// A pool's view says what snapshots it holds, builds and failed to build,
// each by its key, its plan, and what the pool said of it, and the build
// guest's words in a failure are escaped (ADR 26-11-17-316 §7).
func TestWritePoolShowsItsSnapshots(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := app.writePool(cmd, poolWithSnapshots(reportedSnapshots)); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	for _, want := range []string{
		"2 held, 2 building, 1 failed",
		"SNAPSHOT",
		"3f2a91c0e4d1  held",
		"discobox-platform@aaaaaaaaaaaa discobox-harness-shell@bbbbbbbbbbbb off: docker,brew",
		"3.0GiB, 2 boxes",
		"(not recorded)",
		"9b9b9b9b9b9b  building",
		"in snap-9b9b9b9b, docker: install\\x1b[2J, 2/3 done",
		"7c7c7c7c7c7c  queued",
		"e1e1e1e1e1e1  failed",
		`feature go failed`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("pool output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "\x1b") {
		t.Errorf("a build's error reached the terminal raw:\n%q", output)
	}
}

// A pool whose agent did not report its snapshots says so, which is not the
// same as a pool that holds none.
func TestWritePoolTellsNoSnapshotsFromNotReported(t *testing.T) {
	for name, tc := range map[string]struct {
		snapshots, want string
	}{
		"not reported": {"", "not reported"},
		"none":         {`{"held":[],"building":[],"failed":[]}`, "none"},
	} {
		app := &App{output: "table"}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := app.writePool(cmd, poolWithSnapshots(tc.snapshots)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "SNAPSHOTS") || !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "SNAPSHOT  ") {
			t.Errorf("%s: pool output = %q, want SNAPSHOTS %s and no table", name, out.String(), tc.want)
		}
	}
}

// A box's view says what its container or machine was made from.
func TestWriteSandboxShowsWhatItStartedFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		startedFrom, want string
	}{
		"a snapshot": {`,"startedFrom":{"from":"snapshot","snapshot":"3f2a91c0e4d1aaaaaaaa"}`, "snapshot 3f2a91c0e4d1"},
		"the base":   {`,"startedFrom":{"from":"base"}`, "base"},
		"unknown":    {"", "-"},
	} {
		raw := strings.Replace(sandboxRuntime("sandbox-1", "one", ""), `"observedGeneration":1`, `"observedGeneration":1`+tc.startedFrom, 1)
		var sandbox apimodel.Sandbox
		if err := sandbox.UnmarshalJSON([]byte(raw)); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, raw)
		}
		app := &App{output: "table"}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := app.writeSandbox(cmd, &sandbox); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "STARTED FROM") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: sandbox output = %q, want %q", name, out.String(), tc.want)
		}
	}
}

// A box's details name the plan of the snapshot it started from, as its pool
// lists it, and say nothing more when the pool does not (ADR 26-11-17-316 §7).
func TestSandboxGetNamesThePlanItStartedFrom(t *testing.T) {
	box := strings.Replace(sandboxRuntime("sandbox-1", "one", ""), `"observedGeneration":1`, `"observedGeneration":1,"startedFrom":{"from":"snapshot","snapshot":"3f2a91c0e4d1aaaaaaaaaaaa"}`, 1)
	var pool bytes.Buffer
	if err := writeJSON(&pool, poolWithSnapshots(reportedSnapshots)); err != nil {
		t.Fatal(err)
	}
	server := completionServer(t, map[string]string{
		"/projects/project-1/sandboxes":           `{"sandboxes":[` + box + `]}`,
		"/projects/project-1/sandboxes/sandbox-1": box,
		"/projects/project-1/pools/pool-1":        pool.String(),
	})
	out := runCLI(t, server.URL, "admin", "box", "get", "sandbox-1")
	if want := "started from snapshot 3f2a91c0e4d1aaaaaaaaaaaa: discobox-platform@aaaaaaaaaaaa discobox-harness-shell@bbbbbbbbbbbb off: docker,brew"; !strings.Contains(out, want) {
		t.Errorf("box details lack %q:\n%s", want, out)
	}

	gone := strings.Replace(box, "3f2a91c0e4d1aaaaaaaaaaaa", "ffffffffffffffffffffffff", 1)
	server = completionServer(t, map[string]string{
		"/projects/project-1/sandboxes":           `{"sandboxes":[` + gone + `]}`,
		"/projects/project-1/sandboxes/sandbox-1": gone,
		"/projects/project-1/pools/pool-1":        pool.String(),
	})
	if out := runCLI(t, server.URL, "admin", "box", "get", "sandbox-1"); strings.Contains(out, "started from snapshot") || !strings.Contains(out, "snapshot ffffffffffff") {
		t.Errorf("a snapshot its pool no longer lists:\n%s", out)
	}
}

// The pool's JSON carries a build's error escaped too: a C1 control or a
// bidirectional override in it reaches no terminal raw.
func TestWritePoolJSONEscapesABuildsError(t *testing.T) {
	app := &App{output: "json"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	hostile := strings.Replace(reportedSnapshots, `\u001b[31mfeature go failed`, `\u009b31m\u202efeature go failed`, 1)
	if err := app.writePool(cmd, poolWithSnapshots(hostile)); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\u009b\u202e\x1b") || !strings.Contains(out.String(), "feature go failed") {
		t.Errorf("pool JSON = %q", out.String())
	}
}
