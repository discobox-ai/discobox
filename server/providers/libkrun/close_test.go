package libkrun

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/x/shorttmp"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

// A server shutdown must power its pool VMs off in order, as StopVM does: the
// guest's Docker may be writing to data.raw and cache.raw (a first box's pulled
// layers, an unpacked bundle), and a SIGKILL is a dirty unmount of both disks
// that later boxes in the pool inherit.
func TestCloseAsksEachGuestToPowerOffBeforeKillingIt(t *testing.T) {
	dir := shorttmp.Dir(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, lifecycleSocketName))
	if err != nil {
		t.Fatal(err)
	}
	guest := &guestVM{runtimeDir: dir, cmd: &exec.Cmd{}, exited: make(chan struct{})}
	var shutdowns atomic.Int32
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/shutdown" {
			shutdowns.Add(1)
			close(guest.exited) // the guest powers off
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	driver := &Driver{vms: map[string]*guestVM{"pool_1": guest}}
	if err := driver.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := shutdowns.Load(); got != 1 {
		t.Fatalf("lifecycle shutdown requests during Close = %d, want 1 (the guest was killed without being asked to power off)", got)
	}
}

// The VMs stop all at once: a guest that takes its time to power off costs the
// shutdown its own wait, not one wait per pool. Each guest here powers off
// only once every guest has been asked, which a one-at-a-time Close never
// reaches.
func TestCloseStopsEveryGuestAtOnce(t *testing.T) {
	const pools = 2
	var asked, sawAll atomic.Int32
	allAsked := make(chan struct{})
	driver := &Driver{vms: map[string]*guestVM{}}
	for i := range pools {
		dir := shorttmp.Dir(t)
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, lifecycleSocketName))
		if err != nil {
			t.Fatal(err)
		}
		guest := &guestVM{runtimeDir: dir, cmd: &exec.Cmd{}, exited: make(chan struct{})}
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
				return
			}
			if asked.Add(1) == pools {
				close(allAsked)
			}
			select {
			case <-allAsked:
				sawAll.Add(1)
			case <-time.After(3 * time.Second):
			}
			close(guest.exited)
		})}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		driver.vms[fmt.Sprintf("pool_%d", i)] = guest
	}

	if err := driver.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sawAll.Load(); got != pools {
		t.Fatalf("guests that powered off after every guest was asked = %d, want %d (Close stopped the VMs one at a time)", got, pools)
	}
	if len(driver.vms) != 0 {
		t.Fatalf("VMs left registered after Close: %d", len(driver.vms))
	}
}

// Close stops the VMs it finds, so a closed driver starts no more: a reconcile
// still holding a replaced provider would otherwise boot a guest that nothing
// stops, on the disks the replacement's own guest mounts. The refusal is the
// pool not taking traffic yet, which a caller retries against the new provider.
func TestClosedDriverStartsNoVM(t *testing.T) {
	driver := &Driver{vms: map[string]*guestVM{}, stateDir: shorttmp.Dir(t), runtimeDir: shorttmp.Dir(t)}
	if err := driver.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, started, err := driver.launch(t.Context(), "pool_1", nil, nil, vmsize.Size{}, pooloutbound.Resolved{})
	if !errors.Is(err, sandbox.ErrPoolNotReachable) || started {
		t.Fatalf("launch after Close = started %v, %v; want ErrPoolNotReachable", started, err)
	}
	if len(driver.vms) != 0 {
		t.Fatalf("VMs registered after Close: %d", len(driver.vms))
	}
}
