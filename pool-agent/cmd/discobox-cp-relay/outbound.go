package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pkg/wire"
)

func outboundRelay(config string) error {
	raw, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	var settings pooloutbound.RelaySettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errors := make(chan error, 4)
	for i, upstream := range []string{settings.HTTP, settings.HTTPS} {
		if upstream == "" {
			continue
		}
		// The server renders the upstream, and with it the transport: the
		// relay never assumes one (ADR 26-11-17-182 §5).
		endpoint, err := wire.Parse(upstream)
		if err != nil {
			return err
		}
		dial, err := endpoint.DialContext()
		if err != nil {
			return err
		}
		for _, host := range []string{"127.0.0.1", settings.Bridge} {
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(host, []string{"17008", "17009"}[i]))
			if err != nil {
				return err
			}
			go func() {
				errors <- pooloutbound.ServeRelay(ctx, listener, func(ctx context.Context) (net.Conn, error) {
					return dial(ctx, "", "")
				})
			}()
		}
	}
	if socket := os.Getenv("NOTIFY_SOCKET"); socket != "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unixgram", socket)
		if err != nil {
			return err
		}
		_, err = conn.Write([]byte("READY=1"))
		conn.Close()
		if err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errors:
		return err
	}
}

func outboundProbe() error {
	raw, err := os.ReadFile(pooloutbound.RuntimeFile)
	if err != nil {
		return err
	}
	var resolved pooloutbound.Resolved
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(pooloutbound.Check(context.Background(), resolved.WithInternalHosts()))
}
