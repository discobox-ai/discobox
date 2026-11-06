package pooloutbound

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"
)

const HostPort uint32 = 3005
const ContainerPort uint32 = 3008
const LogicalForwarderHost = "discobox-host-forwarder"

// ServeHost is scoped by its backend's per-pool private transport. A guest can
// fetch its boot config and splice bytes only to its two configured upstreams.
func ServeHost(ctx context.Context, listener net.Listener, r Resolved) error {
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			reader := bufio.NewReaderSize(conn, 128)
			op, err := reader.ReadSlice('\n')
			if err != nil || len(op) > 16 {
				return
			}
			if string(op) == "config\n" {
				if err := json.NewEncoder(conn).Encode(r); err != nil {
					return
				}
				return
			}
			var raw string
			switch string(op) {
			case "http\n":
				raw = r.HTTP
			case "https\n":
				raw = r.HTTPS
			default:
				return
			}
			if raw == "" {
				return
			}
			u, err := url.Parse(raw)
			if err != nil {
				return
			}
			address := u.Host
			if u.Port() == "" {
				address = net.JoinHostPort(u.Hostname(), "80")
			}
			dialer := net.Dialer{Timeout: 10 * time.Second}
			target, err := dialer.DialContext(ctx, "tcp", address)
			if err != nil {
				return
			}
			defer target.Close()
			_ = conn.SetDeadline(time.Time{})
			splice(ctx, conn, target, reader)
		}()
	}
}

// ReadHostConfig reads private material before the guest starts dockerd.
func ReadHostConfig(ctx context.Context, dial func(context.Context) (net.Conn, error)) (Resolved, error) {
	conn, err := dial(ctx)
	if err != nil {
		return Resolved{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, "config\n"); err != nil {
		return Resolved{}, err
	}
	var r Resolved
	err = json.NewDecoder(io.LimitReader(conn, 8<<20)).Decode(&r)
	return r, err
}

// RelayEndpoint keeps Basic credentials at the HTTP client, while translating
// only the address. The relay itself never interprets the HTTP protocol.
func RelayEndpoint(raw, address string) string {
	if raw == "" {
		return ""
	}
	u, _ := url.Parse(raw)
	u.Host = address
	return u.String()
}

func ServeGuestRelay(ctx context.Context, listener net.Listener, dial func(context.Context) (net.Conn, error), scheme string) error {
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		source, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer source.Close()
			target, err := dial(ctx)
			if err != nil {
				return
			}
			defer target.Close()
			if _, err := io.WriteString(target, scheme+"\n"); err != nil {
				return
			}
			splice(ctx, source, target, source)
		}()
	}
}

func splice(ctx context.Context, a, b net.Conn, source io.Reader) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = a.Close()
			_ = b.Close()
		case <-done:
		}
	}()
	defer close(done)
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst net.Conn, src io.Reader) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyHalf(b, source)
	go copyHalf(a, b)
	wg.Wait()
}

// ReadContainerConfig waits for the guest's dynamic bridge listeners before a
// pool container can be created. It never substitutes a host loopback address.
func ReadContainerConfig(ctx context.Context, conn net.Conn) (GuestDelivery, error) {
	defer conn.Close()
	deadline := time.Now().Add(60 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	var r GuestDelivery
	if err := json.NewDecoder(io.LimitReader(conn, 8<<20)).Decode(&r); err != nil {
		return r, fmt.Errorf("guest outbound bridge readiness: %w", err)
	}
	return r, nil
}

// GuestDelivery reports both translated routing and the guest daemon context probe.
type GuestDelivery struct {
	Runtime Resolved `json:"runtime"`
	Probes  Probes   `json:"probes"`
}
