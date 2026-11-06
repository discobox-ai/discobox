package pooloutbound

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"
)

const HTTPPort uint32 = 3005
const HTTPSPort uint32 = 3006
const LogicalForwarderHost = "discobox-host-forwarder"

// ServeRelay forwards bytes to one fixed destination. Configuration is delivered
// through the provider's copy/exec path, never through this traffic transport.
func ServeRelay(ctx context.Context, listener net.Listener, dial func(context.Context) (net.Conn, error)) error {
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
			splice(ctx, source, target, source)
		}()
	}
}

// UpstreamDialer connects only to the configured upstream, never a guest-supplied address.
func UpstreamDialer(raw string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("no upstream configured")
		}
		address := u.Host
		if u.Port() == "" {
			address = net.JoinHostPort(u.Hostname(), "80")
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	}
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

// RelaySettings contains addresses only; credentials and trust are placed
// separately in the consuming host/container's private configuration.
type RelaySettings struct {
	HTTP   bool   `json:"http"`
	HTTPS  bool   `json:"https"`
	Bridge string `json:"bridge"`
}
