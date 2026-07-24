// Command discobox-vsock-guest provides the two small guest-side services the
// local libkrun provider needs before the pool-agent container exists:
// Docker's Unix socket over VSOCK and orderly poweroff over VSOCK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	guestvsock "github.com/obot-platform/discobox/pool-agent/vsock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: discobox-vsock-guest <docker-proxy|lifecycle> [flags]")
	}
	switch args[0] {
	case "docker-proxy":
		flags := flag.NewFlagSet("docker-proxy", flag.ContinueOnError)
		port := flags.Uint("port", 3004, "guest VSOCK port")
		socket := flags.String("socket", "/var/run/docker.sock", "Docker Unix socket")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		listener, err := guestvsock.Listen(uint32(*port))
		if err != nil {
			return err
		}
		defer listener.Close()
		return serveUnixProxy(ctx, listener, *socket)
	case "lifecycle":
		flags := flag.NewFlagSet("lifecycle", flag.ContinueOnError)
		port := flags.Uint("port", 3003, "guest VSOCK port")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		listener, err := guestvsock.Listen(uint32(*port))
		if err != nil {
			return err
		}
		defer listener.Close()
		return serveLifecycle(ctx, listener, poweroff)
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func serveUnixProxy(ctx context.Context, listener net.Listener, socket string) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
		go proxyToUnix(ctx, conn, socket)
	}
}

func proxyToUnix(ctx context.Context, source net.Conn, socket string) {
	defer source.Close()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	target, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return
	}
	defer target.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	copyStream := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
	}
	go copyStream(target, source)
	go copyStream(source, target)
	wg.Wait()
}

func serveLifecycle(ctx context.Context, listener net.Listener, shutdown func(context.Context) error) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go func() {
			time.Sleep(50 * time.Millisecond)
			if err := shutdown(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "poweroff:", err)
			}
		}()
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func poweroff(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "poweroff") //nolint:gosec // Fixed guest lifecycle command.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl poweroff: %w: %s", err, string(output))
	}
	return nil
}
