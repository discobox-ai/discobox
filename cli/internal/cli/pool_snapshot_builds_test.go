package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// A build is named by its key or the start of one, as the tables show it,
// among the builds and failures its pool reports.
func TestResolveSnapshotBuildKey(t *testing.T) {
	keys := []string{"3f9a2c1d0e4b77", "3f9a2c1d0e4c88", "9b9b9b9b9b9b"}
	for value, want := range map[string]string{"3f9a2c1d0e4b77": "3f9a2c1d0e4b77", "3f9a2c1d0e4b": "3f9a2c1d0e4b77", "9b": "9b9b9b9b9b9b"} {
		if got, err := resolveSnapshotBuildKey(value, keys); err != nil || got != want {
			t.Errorf("resolve %q = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := resolveSnapshotBuildKey("3f9a", keys); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("an ambiguous prefix: %v", err)
	}
	if _, err := resolveSnapshotBuildKey("aa", keys); err == nil || !strings.Contains(err.Error(), "no snapshot build") {
		t.Errorf("a key no build has: %v", err)
	}
}

// A build's logs are read at the canonical pool route, with the feature,
// count and follow asked for, and what the build wrote is escaped, its last
// line kept though no newline ends it.
func TestStreamSnapshotBuildLogsReadsTheCanonicalRouteAndEscapes(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte("==> /var/log/discobox/provision/docker.log <==\nfetching\x1b[2J\n\tinstalling"))
	}))
	defer server.Close()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	var out bytes.Buffer
	if err := app.streamSnapshotBuildLogs(context.Background(), "proj_1", "pool_1", "abc", "docker", 30, true, &out); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/projects/proj_1/pools/pool_1/snapshot-builds/abc/logs" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery != "feature=docker&follow=true&tail=30" {
		t.Fatalf("query = %q", gotQuery)
	}
	if want := "==> /var/log/discobox/provision/docker.log <==\nfetching\\x1b[2J\n\tinstalling"; out.String() != want {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}

// A refusal reaches the operator with the server's reason.
func TestStreamSnapshotBuildLogsSurfacesTheRefusal(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the snapshot build is queued and has no box yet"}`))
	}))
	defer server.Close()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	err := app.streamSnapshotBuildLogs(context.Background(), "proj_1", "pool_1", "abc", "", 0, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "queued") {
		t.Fatalf("err = %v", err)
	}
}

// An exec in a build runs over the build's route, its command sent, its
// output and exit status returned.
func TestSnapshotBuildExecRunsOverTheBuildsRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var gotPath string
	var gotCommand []string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCommand = r.URL.Path, r.URL.Query()["command"]
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer socket.CloseNow()
		conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)
		for {
			next, err := frame.Read(conn)
			if err != nil || next.Type == frame.CloseInput {
				break
			}
		}
		_ = frame.Write(conn, frame.Stdout, []byte("PID 1\n"))
		code := int64(0)
		payload, _ := frame.EncodeExit("exited", &code, "")
		_ = frame.Write(conn, frame.Exit, payload)
	}))
	defer server.Close()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	route := poolExecRoute{path: snapshotBuildPath("proj_1", "pool_1", "abc", "exec"), kind: "snapshot build", where: "the build's box"}
	var out bytes.Buffer
	if err := app.attachPoolExec(ctx, route, []string{"ps", "aux"}, false, strings.NewReader(""), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/projects/proj_1/pools/pool_1/snapshot-builds/abc/exec" || strings.Join(gotCommand, " ") != "ps aux" || out.String() != "PID 1\n" {
		t.Fatalf("path %q, command %q, out %q", gotPath, gotCommand, out.String())
	}
}

// The builds listed beside the boxes show their pool, key, state, box and
// what they are provisioning, the build's words escaped.
func TestWriteSnapshotBuildsShowsEachBuildsBoxAndProgress(t *testing.T) {
	pool := poolWithSnapshots(reportedSnapshots)
	_, snapshots := poolSnapshotsSummary(*pool)
	if snapshots == nil {
		t.Fatal("no snapshots")
	}
	var builds []poolSnapshotBuild
	for _, build := range snapshots.Building {
		builds = append(builds, poolSnapshotBuild{Pool: "pool_1", Build: build})
	}
	var out bytes.Buffer
	if err := writeSnapshotBuilds(&out, builds, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"POOL    SNAPSHOT",
		"pool_1  9b9b9b9b9b9b  building  started 4 minutes ago  snap-9b9b9b9b  docker: install\\x1b[2J, 2/3 done",
		"pool_1  7c7c7c7c7c7c  queued    asked 2 minutes ago    -",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Error("a build's words reached the terminal unescaped")
	}
}

// A build that has reported nothing shows so, and one done provisioning its
// features its state.
func TestSnapshotBuildProgressSaysWhatIsUnderWay(t *testing.T) {
	if got := snapshotBuildProgress(apimodel.PoolSnapshotBuild{}.Progress); got != "-" {
		t.Errorf("no progress = %q", got)
	}
	var build apimodel.PoolSnapshotBuild
	if err := build.UnmarshalJSON([]byte(`{"key":"k","requestedAt":"2026-08-27T11:55:00Z","plan":{"base":"b"},"progress":{"state":"provisioning","features":{"go":{"state":"failed"},"nix":{"state":"provisioning"}}}}`)); err != nil {
		t.Fatal(err)
	}
	if got := snapshotBuildProgress(build.Progress); got != "nix, go failed, 1/2 done" {
		t.Errorf("progress = %q", got)
	}
}

// A log's text is shown as it comes, a line redrawn on a carriage return
// included, and a rune cut in two by a read is held only until its end
// arrives.
func TestTerminalSafeWriterShowsTextAsItComes(t *testing.T) {
	var out bytes.Buffer
	w := &terminalSafeWriter{w: &out}
	_, _ = w.Write([]byte(" 10%\r 20%"))
	if out.String() != ` 10%\r 20%` {
		t.Fatalf("a redrawn line showed %q, want it at once", out.String())
	}
	out.Reset()
	euro := []byte("€")
	_, _ = w.Write(euro[:1])
	if out.Len() != 0 {
		t.Fatalf("half a rune showed as %q", out.String())
	}
	_, _ = w.Write(append(euro[1:], '\n'))
	if out.String() != "€\n" {
		t.Fatalf("the rune showed as %q", out.String())
	}
}
