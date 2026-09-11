//go:build darwin && cgo

package vzvm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The stress test boots a real guest, so it is opt-in and needs a test binary
// signed with the Virtualization entitlement: run it through
// `go tool task test:vz-stress`, never `go test`.
const (
	stressGuestEnv     = "DISCOBOX_VZ_STRESS_GUEST"
	stressNoVMEnv      = "DISCOBOX_VZ_STRESS_NO_VM"
	stressArtifactsEnv = "DISCOBOX_VZ_STRESS_ARTIFACTS"
	stressCyclesEnv    = "DISCOBOX_VZ_STRESS_CYCLES"
	stressDurationEnv  = "DISCOBOX_VZ_STRESS_DURATION"
	stressWorkersEnv   = "DISCOBOX_VZ_STRESS_WORKERS"
	stressPauseEnv     = "DISCOBOX_VZ_STRESS_PAUSE"

	// stressPauseAfter is how long the churn runs before a requested pause.
	stressPauseAfter = 10 * time.Second
	// stressClockSettled is how close the guest clock must come back to the
	// host's, and stressClockWatch how long to wait for it: the guest steps its
	// clock from the RTC every 30s.
	stressClockSettled = 2 * time.Second
	stressClockWatch   = 90 * time.Second

	// stressDockerPort is the guest's Docker socket relay. It answers as soon
	// as the guest's daemon is up, with no pool agent needed.
	stressDockerPort = 3004
	// stressKernelCmdline mirrors the vz driver's kernelCmdline, which this
	// package cannot import.
	stressKernelCmdline = "console=hvc0 root=/dev/vda ro rootfstype=ext4"
)

// TestVSOCKChurnKeepsUnixListenerServing holds a Unix HTTP listener open,
// allocated before the VM exactly as the server's is, and probes it on fresh
// connections while host-to-guest VSOCK connections are opened and closed.
// Serve returning at all is the incident: the server treats any Accept error
// as fatal and exits.
//
// With a pause requested, the guest is paused through the framework partway
// through, the way a Mac's sleep stops it, and the report says how far its
// clock fell behind and how quickly it caught up.
func TestVSOCKChurnKeepsUnixListenerServing(t *testing.T) {
	guestDir := os.Getenv(stressGuestEnv)
	noVM := os.Getenv(stressNoVMEnv) != ""
	if guestDir == "" && !noVM {
		t.Skipf("set %s to a guest image directory, or %s=1 for the control without a VM", stressGuestEnv, stressNoVMEnv)
	}
	cycles := stressEnvInt(t, stressCyclesEnv, 10000)
	limit := stressEnvDuration(t, stressDurationEnv, 30*time.Minute)
	workers := stressEnvInt(t, stressWorkersEnv, 8)
	pause := stressEnvDuration(t, stressPauseEnv, 0)
	if pause > 0 && noVM {
		t.Fatalf("%s needs a guest to pause", stressPauseEnv)
	}

	// $TMPDIR on macOS is long enough that a test-named directory under it can
	// overflow sun_path's 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "vzs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s.sock")
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	var vm *VM
	if !noVM {
		vm = startStressGuest(t, guestDir, dir)
		waitStressDocker(t, vm, 5*time.Minute)
	}

	baseFDs := openFDCount()
	t.Logf("pid=%d fds=%d cycles=%d limit=%s workers=%d vm=%t pause=%s socket=%s", os.Getpid(), baseFDs, cycles, limit, workers, vm != nil, pause, sock)

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()

	var (
		vsockCycles, vsockErrors atomic.Int64
		probes, probeErrors      atomic.Int64
		errMu                    sync.Mutex
		errs                     []string
	)
	record := func(kind string, err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if len(errs) < 500 {
			errs = append(errs, fmt.Sprintf("%s %s: %v", time.Since(start).Round(time.Millisecond), kind, err))
		}
	}

	serveFailed := make(chan error, 1)
	go func() {
		select {
		case err := <-serveErr:
			record("serve", err)
			serveFailed <- err
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	for range workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				n := probes.Add(1)
				if vm == nil && n > int64(cycles) {
					cancel()
					return
				}
				if err := probeStressListener(ctx, client); err != nil && ctx.Err() == nil {
					probeErrors.Add(1)
					record("probe", err)
				}
			}
		})
	}
	var pauseReport string
	if vm != nil {
		for worker := range workers {
			wg.Go(func() {
				for ctx.Err() == nil {
					n := vsockCycles.Add(1)
					if n > int64(cycles) {
						cancel()
						return
					}
					// Every fourth connection is dropped with its request in
					// flight, the way a canceled caller leaves one.
					abrupt := (int(n)+worker)%4 == 0
					if err := pingStressDocker(vm, abrupt); err != nil {
						vsockErrors.Add(1)
						record("vsock", err)
						// A paused guest refuses every connection at once;
						// backing off keeps that from being a spin.
						time.Sleep(10 * time.Millisecond)
					}
				}
			})
		}
		if pause > 0 {
			wg.Go(func() {
				select {
				case <-ctx.Done():
					pauseReport = "skipped: the churn ended before the pause was due"
					return
				case <-time.After(stressPauseAfter):
				}
				pauseReport = pauseStressGuest(vm, pause)
			})
		}
	}
	wg.Wait()

	var failure error
	select {
	case failure = <-serveFailed:
	default:
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	peakFDs := openFDCount()
	if vm != nil {
		_ = vm.Close()
	}
	if pauseReport == "" {
		pauseReport = "off"
	}
	errMu.Lock()
	summary := fmt.Sprintf("pid=%d elapsed=%s vm=%t workers=%d\nvsock cycles=%d errors=%d\nunix probes=%d errors=%d\nfds before=%d after churn=%d after vm close=%d\npause: %s\nserve failure: %v\n\nerrors (first %d):\n%s\n",
		os.Getpid(), elapsed, vm != nil, workers,
		min(vsockCycles.Load(), int64(cycles)), vsockErrors.Load(),
		probes.Load(), probeErrors.Load(),
		baseFDs, peakFDs, openFDCount(),
		pauseReport,
		failure, len(errs), strings.Join(errs, "\n"))
	errMu.Unlock()
	t.Log(summary)
	writeStressArtifacts(t, os.Getenv(stressArtifactsEnv), dir, summary, failure != nil)
	if failure != nil {
		t.Fatalf("the Unix listener stopped serving after %s: %v", elapsed, failure)
	}
}

func startStressGuest(t *testing.T, guestDir, dir string) *VM {
	t.Helper()
	data, cache := filepath.Join(dir, "data.raw"), filepath.Join(dir, "cache.raw")
	for path, size := range map[string]int64{data: 16 << 30, cache: 4 << 30} {
		if err := CreateDiskImage(path, size); err != nil {
			t.Fatal(err)
		}
	}
	vm, err := Start(Options{
		Name:           "vz-stress",
		CPUCount:       4,
		MemoryBytes:    4 << 30,
		KernelPath:     filepath.Join(guestDir, "vmlinux"),
		InitrdPath:     filepath.Join(guestDir, "initrd.img"),
		KernelCmdline:  stressKernelCmdline,
		RootImagePath:  filepath.Join(guestDir, "root.ext4"),
		DataImagePath:  data,
		CacheImagePath: cache,
		ConsoleLogPath: filepath.Join(dir, "console.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	return vm
}

// waitStressDocker waits for the guest's Docker daemon to answer through the
// VSOCK relay. Each attempt is bounded separately because a connect to a port
// nothing in the guest listens on yet may never complete.
func waitStressDocker(t *testing.T, vm *VM, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		done := make(chan error, 1)
		go func() { done <- pingStressDocker(vm, false) }()
		select {
		case last = <-done:
			if last == nil {
				return
			}
		case <-time.After(15 * time.Second):
			last = errors.New("connect did not complete")
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("guest Docker did not answer over VSOCK within %s: %v", timeout, last)
}

// pauseStressGuest pauses the guest through the framework for hold, resumes
// it, and samples the guest clock against the host's until the guest has
// stepped it back. It reports rather than fails: the listener is what the test
// asserts on, and the clock is evidence for the sleep/wake investigation.
func pauseStressGuest(vm *VM, hold time.Duration) string {
	vm.mu.Lock()
	machine := vm.machine
	vm.mu.Unlock()
	if machine == nil || !machine.CanPause() {
		return "the framework would not pause the guest"
	}
	before, err := stressGuestClockLag(vm)
	if err != nil {
		return fmt.Sprintf("clock before pause: %v", err)
	}
	if err := machine.Pause(); err != nil {
		return fmt.Sprintf("pause: %v", err)
	}
	time.Sleep(hold)
	if err := machine.Resume(); err != nil {
		return fmt.Sprintf("resume: %v", err)
	}
	resumed := time.Now()
	var samples []string
	settled := "not within " + stressClockWatch.String()
	for time.Since(resumed) < stressClockWatch {
		lag, err := stressGuestClockLag(vm)
		at := time.Since(resumed).Round(100 * time.Millisecond)
		if err != nil {
			samples = append(samples, fmt.Sprintf("+%s %v", at, err))
		} else {
			samples = append(samples, fmt.Sprintf("+%s %s", at, lag.Round(100*time.Millisecond)))
			if lag.Abs() <= stressClockSettled {
				settled = at.String()
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Sprintf("held %s; guest behind host by %s before; after resume %s; within %s after %s",
		hold, before.Round(100*time.Millisecond), strings.Join(samples, ", "), stressClockSettled, settled)
}

// stressGuestClockLag reads the guest's clock from its Docker daemon and
// returns how far it is behind the host's, measured against the midpoint of
// the request.
func stressGuestClockLag(vm *VM) (time.Duration, error) {
	conn, err := vm.Connect(stressDockerPort)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	sent := time.Now()
	if _, err := io.WriteString(conn, "GET /info HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n"); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct{ SystemTime time.Time }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return 0, err
	}
	received := time.Now()
	return sent.Add(received.Sub(sent) / 2).Sub(info.SystemTime), nil
}

func pingStressDocker(vm *VM, abrupt bool) error {
	conn, err := vm.Connect(stressDockerPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	if abrupt {
		return nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker ping: %s", resp.Status)
	}
	return nil
}

func probeStressListener(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://stress/", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: %s", resp.Status)
	}
	return nil
}

func openFDCount() int {
	entries, _ := os.ReadDir("/dev/fd")
	return len(entries)
}

func writeStressArtifacts(t *testing.T, root, workDir, summary string, failed bool) {
	t.Helper()
	if root == "" {
		return
	}
	run := filepath.Join(root, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(run, 0o750); err != nil {
		t.Logf("artifacts: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(run, "summary.txt"), []byte(summary), 0o600)
	if console, err := os.ReadFile(filepath.Join(workDir, "console.log")); err == nil {
		_ = os.WriteFile(filepath.Join(run, "console.log"), console, 0o600)
	}
	if failed {
		if f, err := os.Create(filepath.Join(run, "goroutines.txt")); err == nil {
			_ = pprof.Lookup("goroutine").WriteTo(f, 2)
			_ = f.Close()
		}
	}
	t.Logf("artifacts: %s", run)
}

func stressEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		t.Fatalf("%s=%q is not a positive integer", name, raw)
	}
	return value
}

func stressEnvDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s=%q is not a positive duration", name, raw)
	}
	return value
}
