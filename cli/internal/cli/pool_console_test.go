package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

func TestPoolConsoleCommandParsing(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		pool    string
		command []string
	}{
		{args: nil},
		{args: []string{"pool_1"}, pool: "pool_1"},
		{args: []string{"--", "docker", "ps", "-a"}, command: []string{"docker", "ps", "-a"}},
		{args: []string{"pool_1", "--", "sh", "-c", "echo a b", "--"}, pool: "pool_1", command: []string{"sh", "-c", "echo a b", "--"}},
		{args: []string{"pool_1", "docker", "ps", "-a"}, pool: "pool_1", command: []string{"docker", "ps", "-a"}},
		{args: []string{"pool_1", "sh", "-c", "echo a b", "--"}, pool: "pool_1", command: []string{"sh", "-c", "echo a b", "--"}},
		{args: []string{"-t", "pool_1", "--", "docker", "ps", "-a"}, pool: "pool_1", command: []string{"docker", "ps", "-a"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cmd := (&App{}).newPoolConsoleCommand()
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			line, err := splitTargetCommand(cmd, cmd.Flags().Args())
			if err != nil {
				t.Fatal(err)
			}
			if line.target != tt.pool || !slices.Equal(line.command, tt.command) {
				t.Fatalf("got %q %q", line.target, line.command)
			}
		})
	}
}

// Exercise the real attach client, including EOF and nonzero exit, without
// leaking console narration or terminal cleanup into redirected output.
func TestPoolConsoleCommandStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	received := make(chan string, 1)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["command"]; !reflect.DeepEqual(got, []string{"sh", "-c", "echo a b"}) || r.URL.Query().Get("tty") != "false" {
			t.Errorf("query = %v", r.URL.Query())
		}
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer socket.CloseNow()
		conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)
		var input strings.Builder
		for {
			f, err := frame.Read(conn)
			if err != nil {
				t.Error(err)
				return
			}
			if f.Type == frame.Input {
				input.Write(f.Payload)
			}
			if f.Type == frame.CloseInput {
				break
			}
		}
		received <- input.String()
		_ = frame.Write(conn, frame.Stdout, []byte("out\n"))
		_ = frame.Write(conn, frame.Stderr, []byte("err\n"))
		code := int64(37)
		payload, _ := frame.EncodeExit("exited", &code, "")
		_ = frame.Write(conn, frame.Exit, payload)
	}))
	defer server.Close()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	var out, errout bytes.Buffer
	err := app.attachPoolConsole(ctx, "proj_1", "pool_1", []string{"sh", "-c", "echo a b"}, false, strings.NewReader("piped input\n"), &out, &errout)
	if code, ok := ExitCode(err); !ok || code != 37 {
		t.Fatalf("exit=%v", err)
	}
	if out.String() != "out\n" || errout.String() != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errout.String())
	}
	if input := <-received; input != "piped input\n" {
		t.Fatalf("stdin=%q", input)
	}
}

func TestPoolConsoleCommandRequiresCompletion(t *testing.T) {
	for _, outcome := range []string{"EOF", "detached", "provider-error"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer socket.Close(websocket.StatusNormalClosure, "done")
				conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)
				// Drain client input before ending so a concurrent input write cannot
				// hide the missing-completion response this test exercises.
				for {
					next, err := frame.Read(conn)
					if err != nil {
						return
					}
					if next.Type == frame.CloseInput {
						break
					}
				}
				switch outcome {
				case "detached":
					payload, _ := frame.EncodeExit("detached", nil, "")
					_ = frame.Write(conn, frame.Exit, payload)
				case "provider-error":
					_ = frame.Write(conn, frame.Error, []byte("pool host unavailable"))
				}
			}))
			defer server.Close()
			app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
			err := app.attachPoolConsole(ctx, "proj_1", "pool_1", []string{"true"}, false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("incomplete/failed command returned success")
			}
			if outcome == "provider-error" && !strings.Contains(err.Error(), "pool host unavailable") {
				t.Fatalf("error=%v", err)
			}
			if outcome != "provider-error" && !strings.Contains(err.Error(), "without an exit status") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
