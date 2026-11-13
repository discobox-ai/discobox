package poolruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/pool-agent/createprotocol"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

type channelCreateRuntime struct {
	channelRuntime
	token string
}

func (r channelCreateRuntime) AcquirePoolAgentClient(context.Context, *model.Pool) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(http.DefaultClient, r.url, func(context.Context) (string, error) { return r.token, nil }, nil), nil
}

// An egress connection loss can take both the control-plane channel and the
// separate create WebSocket with it, after the runtime has created the sandbox.
// The holder must redial, and create must rediscover and recover the same ID.
func TestAnInFlightCreateSurvivesAChannelRedial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, serverKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	poolPublic, poolKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := cpchannel.New(slog.New(slog.DiscardHandler), poolKey, serverKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortTempDir(t), "cp.sock")
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go channel.Serve(ctx, listener)
	// Probe the pool's actual control-plane socket while dispatching each create.
	// The retry's probe must wait for the replacement channel when necessary.
	probe := func(ctx context.Context) error {
		var dial net.Dialer
		conn, err := dial.DialContext(ctx, "unix", socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if _, err = io.WriteString(conn, "ping"); err != nil {
			return err
		}
		var reply [4]byte
		if _, err = io.ReadFull(conn, reply[:]); err != nil {
			return err
		}
		if string(reply[:]) != "pong" {
			return fmt.Errorf("control plane replied %q", reply)
		}
		return nil
	}
	memory := sandboxruntime.NewMemorySandboxRuntime()
	authKey, token := newPoolAgentTestAuth(t, "project-1", "pool-1", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{Identity: poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"}, Runtime: memory, ControlPlanePublicKey: authKey})
	if err != nil {
		t.Fatal(err)
	}
	var attempts, opens, discoveries atomic.Int32
	committed := make(chan struct{})
	createHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes") {
			router.ServeHTTP(w, r)
			return
		}
		attempt := attempts.Add(1)
		if err := probe(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if attempt != 1 {
			router.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Errorf("initial create: %d %s", response.Code, response.Body)
			return
		}
		close(committed)
		// No response reaches the client, although runtime creation finished.
		<-r.Context().Done()
	})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cpchannel.Path("project-1", "pool-1"):
			opens.Add(1)
			channel.ServeHTTP(w, r)
		case createprotocol.Path("project-1", "pool-1"):
			discoveries.Add(1)
			createprotocol.Serve(w, r, createHandler)
		default:
			router.ServeHTTP(w, r)
		}
	}))
	defer agent.Close()
	target, _ := url.Parse(agent.URL)
	edge := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
	var mu sync.Mutex
	hijacked := map[net.Conn]struct{}{}
	edge.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		if state == http.StateHijacked {
			hijacked[conn] = struct{}{}
		}
	}
	edge.Start()
	defer edge.Close()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for c := range hijacked {
			c.Close()
		}
	}()
	pool := activePool("pool-1")
	pool.ProjectID = "project-1"
	pool.PublicKey = base64.StdEncoding.EncodeToString(poolPublic)
	manager := &fakePoolManager{pool: pool, schedulable: true, issuerKey: serverKey}
	sink := recordingSink{pushed: make(chan string, 4)}
	provider := New(channelCreateRuntime{channelRuntime: channelRuntime{url: edge.URL}, token: token}, sandbox.ProviderDefinition{Name: "test"}, manager, sink)
	defer provider.Close()
	provider.holdChannel(pool)
	if err := channel.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		sb    *sandbox.Sandbox
		state []byte
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		sb, state, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
		finished <- result{sb, state, err}
	}()
	select {
	case <-committed:
	case <-ctx.Done():
		t.Fatal("create never reached the runtime")
	}
	mu.Lock()
	cut := make([]net.Conn, 0, len(hijacked))
	for conn := range hijacked {
		cut = append(cut, conn)
	}
	mu.Unlock()
	if len(cut) != 2 {
		t.Fatalf("upgraded connections=%d, want channel and create", len(cut))
	}
	for _, conn := range cut {
		conn.Close()
	}
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatalf("create across redial: %v", got.err)
		}
		if got.sb == nil || got.sb.SandboxID != "sandbox-1" || len(got.state) == 0 {
			t.Fatalf("recovered create: %#v state=%s", got.sb, got.state)
		}
	case <-ctx.Done():
		t.Fatal("create never recovered")
	}
	if opens.Load() < 2 || discoveries.Load() != 2 || attempts.Load() != 2 {
		t.Fatalf("channel opens=%d discoveries=%d create attempts=%d", opens.Load(), discoveries.Load(), attempts.Load())
	}
	if err := probe(ctx); err != nil {
		t.Fatalf("pool control plane after recovery: %v", err)
	}
	instances, err := memory.ListSandboxes(ctx)
	if err != nil || len(instances) != 1 || instances[0].SandboxID != "sandbox-1" {
		t.Fatalf("runtime instances after recovery: %#v, %v", instances, err)
	}
}
