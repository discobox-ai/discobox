package poolruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// channelRuntime reaches one pool agent's API at url, which is all a
// control-plane channel asks of a runtime.
type channelRuntime struct {
	RuntimeProvider
	url string
}

func (r channelRuntime) AcquirePoolAgentClient(context.Context, *model.Pool) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, r.url, nil), nil
}

func (channelRuntime) Close() error { return nil }

// recordingSink is the carrier hub: it records whose each stream is, and
// answers what the pool sent on it.
type recordingSink struct {
	pushed chan string
}

func (s recordingSink) Push(poolID string, conn net.Conn, _ <-chan struct{}) error {
	go func() {
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		_, _ = conn.Write([]byte("pong"))
		s.pushed <- poolID + " " + string(buf)
	}()
	return nil
}

// A provider whose pools do not dial their control plane holds a channel open
// to each one it reconciles, and every connection the pool's processes make
// to the control plane reaches the carrier hub as that pool's (ADR
// 26-10-30-875 §§1–3).
func TestAProviderHoldsAChannelToItsPoolAndPushesItsStreams(t *testing.T) {
	_, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	poolPublic, poolKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, _ := serverKey.Public().(ed25519.PublicKey)

	// The pool's agent: the channel's pool end at its route, and the socket
	// the pool's processes reach the control plane at.
	channel, err := cpchannel.New(slog.New(slog.DiscardHandler), poolKey, serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(chan string, 4)
	mux := http.NewServeMux()
	mux.HandleFunc(cpchannel.Path("project-1", "pool-1"), func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("Authorization")
		channel.ServeHTTP(w, r)
	})
	agentAPI := httptest.NewServer(mux)
	t.Cleanup(agentAPI.Close)
	socket := filepath.Join(shortTempDir(t), "cp.sock")
	var listen net.ListenConfig
	listener, err := listen.Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = channel.Serve(context.Background(), listener) }()

	pool := activePool("pool-1")
	pool.ProjectID = "project-1"
	pool.PublicKey = base64.StdEncoding.EncodeToString(poolPublic)
	manager := &fakePoolManager{pool: pool, issuerKey: serverKey}
	sink := recordingSink{pushed: make(chan string, 1)}
	provider := New(channelRuntime{url: agentAPI.URL}, sandbox.ProviderDefinition{Name: "test"}, manager, sink)
	t.Cleanup(func() { _ = provider.Close() })

	provider.holdChannel(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("no channel arrived: %v", err)
	}
	if got := <-tokens; got != "Bearer pool-token" {
		t.Fatalf("the channel was asked for with %q, want the server's token", got)
	}

	var dialer net.Dialer
	local, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	if _, err := local.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(local, reply); err != nil || string(reply) != "pong" {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	select {
	case got := <-sink.pushed:
		if got != "pool-1 ping" {
			t.Fatalf("the hub got %q, want pool-1's stream", got)
		}
	case <-ctx.Done():
		t.Fatal("the hub got no stream")
	}

	// Removing the pool closes its channel.
	provider.channels.release(pool.ID)
	closed, cancelClosed := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClosed()
	for {
		probe, err := dialer.DialContext(closed, "unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = probe.Write([]byte("ping"))
		_ = probe.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err = probe.Read(make([]byte, 1))
		_ = probe.Close()
		if err != nil {
			break
		}
		if closed.Err() != nil {
			t.Fatal("the channel stayed open after its pool was released")
		}
	}
}

// A pool reached over a channel is told so in its bootstrap, at the socket its
// agent serves it at; one that dials its control plane is told nothing.
func TestAPoolsBootstrapSaysWhetherItIsReachedOverAChannel(t *testing.T) {
	pool := activePool("pool-1")
	project := &model.Project{ID: "project-1"}
	for _, channel := range []bool{true, false} {
		maker, err := newPoolMaker(&fakePoolManager{pool: pool}, project, pool, channel)
		if err != nil {
			t.Fatal(err)
		}
		bootstrap, err := maker.Bootstrap(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wantURL := ""
		if channel {
			wantURL = poolagent.ControlPlaneChannelURL
		}
		if bootstrap.ControlPlaneChannel != channel || bootstrap.ControlPlaneURL != wantURL {
			t.Fatalf("channel %v: bootstrap names channel %v at %q, want %q", channel, bootstrap.ControlPlaneChannel, bootstrap.ControlPlaneURL, wantURL)
		}
	}
}

// shortTempDir is a temporary directory short enough to hold a Unix socket:
// t.TempDir's path, under a CI runner's temporary root, can pass the ~100
// bytes a socket path is limited to, and bind fails with "invalid argument".
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
