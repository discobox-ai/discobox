package libkrun

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discobox-ai/x/shorttmp"
)

func TestLauncherWaitKeepsLiveOrProgressingBoots(t *testing.T) {
	for _, signal := range []string{"heartbeat", "launcher.log", "console.log"} {
		t.Run(signal, func(t *testing.T) {
			dir := shorttmp.Dir(t)
			synctest.Test(t, func(t *testing.T) {
				alive := make(chan struct{}, 1)
				guest := &guestVM{runtimeDir: dir, alive: alive, exited: make(chan struct{}), sockets: []string{filepath.Join(dir, "guest.sock")}}
				result := make(chan error, 1)
				go func() { result <- (&Driver{}).waitForLauncher(t.Context(), guest) }()
				// The observed large guest needed 238s; stay live beyond that without
				// sockets or guest output. No total-duration limit may cut this short.
				for range 50 {
					time.Sleep(5 * time.Second)
					if signal == "heartbeat" {
						alive <- struct{}{}
					} else {
						f, err := os.OpenFile(filepath.Join(dir, signal), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := f.WriteString("boot progress\n"); err != nil {
							t.Fatal(err)
						}
						if err := f.Close(); err != nil {
							t.Fatal(err)
						}
					}
					time.Sleep(100 * time.Millisecond)
					select {
					case err := <-result:
						t.Fatalf("live boot ended early: %v", err)
					default:
					}
				}
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", guest.sockets[0])
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestLauncherWaitReportsSilenceExitAndCancellation(t *testing.T) {
	for _, outcome := range []string{"silent", "exit", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			dir := shorttmp.Dir(t)
			if err := os.WriteFile(filepath.Join(dir, launcherLogName), []byte("entering VM (krun_start_enter)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				guest := &guestVM{runtimeDir: dir, exited: make(chan struct{}), sockets: []string{filepath.Join(dir, "missing.sock")}}
				result := make(chan error, 1)
				go func() { result <- (&Driver{}).waitForLauncher(ctx, guest) }()
				synctest.Wait()
				switch outcome {
				case "silent":
					time.Sleep(launcherIdleTimeout + time.Second)
				case "exit":
					close(guest.exited)
				case "cancel":
					cancel()
				}
				err := <-result
				if err == nil || !strings.Contains(err.Error(), "krun_start_enter") {
					t.Fatalf("startup error = %v", err)
				}
				if outcome == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				if outcome == "silent" && (!strings.Contains(err.Error(), "no liveness or log progress") || !strings.Contains(err.Error(), "missing.sock")) {
					t.Fatalf("lost stall diagnostics: %v", err)
				}
				if outcome == "exit" && !strings.Contains(err.Error(), "exited during startup") {
					t.Fatalf("lost exit diagnostic: %v", err)
				}
			})
		})
	}
}

func TestExitedLauncherCannotSatisfySocketReadiness(t *testing.T) {
	dir := shorttmp.Dir(t)
	socket := filepath.Join(dir, "guest.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	exited := make(chan struct{})
	close(exited)
	err = (&Driver{}).waitForLauncher(t.Context(), &guestVM{runtimeDir: dir, sockets: []string{socket}, exited: exited})
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("dead launcher reported ready: %v", err)
	}
}

func TestLauncherWaitChecksFreshOutputAtInactivityBoundary(t *testing.T) {
	dir := shorttmp.Dir(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- (&Driver{}).waitForLauncher(ctx, &guestVM{runtimeDir: dir, exited: make(chan struct{}), sockets: []string{filepath.Join(dir, "guest.sock")}})
		}()
		synctest.Wait()
		time.Sleep(launcherIdleTimeout - 50*time.Millisecond)
		if err := os.WriteFile(filepath.Join(dir, consoleLogName), []byte("guest activated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		select {
		case err := <-result:
			t.Fatalf("fresh output lost to inactivity check: %v", err)
		default:
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	})
}
