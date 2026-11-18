package provisioner

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/client"
)

// silentDaemon is a Docker daemon that creates an exec and then, on its
// start, either never answers the upgrade or upgrades and never sends a byte.
// The channel it returns closes once the client has closed the start's
// connection.
func silentDaemon(t *testing.T, upgrade bool) (*client.Client, <-chan struct{}) {
	t.Helper()
	released := make(chan struct{})
	var mu sync.Mutex
	var held []net.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"exec-1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/exec-1/start"):
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if upgrade {
				_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				_ = buf.Flush()
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			go func() {
				_, _ = io.Copy(io.Discard, buf)
				close(released)
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
		srv.Close()
	})
	cli, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	return cli, released
}

func TestContainerExecEndsWithItsContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		upgrade bool
	}{
		{"start never answered", false},
		{"stream never sends", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, released := silentDaemon(t, tc.upgrade)
			g := ContainerGuest{Client: cli, Container: "pool"}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"true"}}, nil, io.Discard, io.Discard)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Run = %v, want the context's deadline", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run still blocked 5s after a 200ms context ended")
			}
			// Giving up is not enough: the exec's connection is closed too, so
			// nothing is left waiting on a daemon that will never answer.
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("the exec's connection was still open 5s after Run returned")
			}
		})
	}
}
