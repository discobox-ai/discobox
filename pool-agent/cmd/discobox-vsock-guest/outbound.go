package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	guestvsock "github.com/discobox-ai/discobox/pkg/vsock"
)

func runOutbound(ctx context.Context) error {
	dial := func(ctx context.Context) (net.Conn, error) {
		return guestvsock.DialContextCID(guestvsock.HostCID, pooloutbound.HostPort)(ctx, "vsock", "")
	}
	var r pooloutbound.Resolved
	var err error
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	for {
		r, err = pooloutbound.ReadHostConfig(ctx, dial)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("pool outbound boot delivery failed: %w", err)
		case <-retry.C:
		}
	}
	guest := r
	ports := []string{"17008", "17009"}
	for index, raw := range []string{r.HTTP, r.HTTPS} {
		if raw == "" {
			continue
		}
		address := net.JoinHostPort("127.0.0.1", ports[index])
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		if err != nil {
			return err
		}
		scheme := []string{"http", "https"}[index]
		go func() { _ = pooloutbound.ServeGuestRelay(ctx, listener, dial, scheme) }()
		if index == 0 {
			guest.HTTP = pooloutbound.RelayEndpoint(raw, address)
		} else {
			guest.HTTPS = pooloutbound.RelayEndpoint(raw, address)
		}
	}
	// The guest root is shared and read-only. Trust is boot-local overlay state.
	for index, target := range []string{"/etc", "/usr/local/share/ca-certificates"} {
		root := fmt.Sprintf("/run/discobox-outbound-overlay/%d", index)
		if err := os.MkdirAll(root+"/upper", 0755); err != nil {
			return err
		}
		if err := os.MkdirAll(root+"/work", 0755); err != nil {
			return err
		}
		//nolint:gosec // Targets are the fixed guest overlay directories above.
		mount := exec.CommandContext(ctx, "mount", "-t", "overlay", "overlay", "-o", "lowerdir="+target+",upperdir="+root+"/upper,workdir="+root+"/work", target)
		if err := mount.Run(); err != nil {
			return fmt.Errorf("prepare guest trust overlay: %w", err)
		}
	}
	files, err := guest.Files()
	if err != nil {
		return err
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			return err
		}
	}
	//nolint:gosec // Fixed private install script delivered over the pool boot channel.
	command := exec.CommandContext(ctx, "sh", pooloutbound.InstallFile)
	command.Env = append(os.Environ(), pooloutbound.RevisionEnv+"="+guest.Revision())
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	probes := pooloutbound.Check(ctx, guest.WithInternalHosts())
	// Readiness permits dockerd to start. Its registry pulls use guest loopback.
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
	// Docker creates docker0 only after its daemon starts. Publish only after
	// all required listeners are bound to that discovered address.
	containerListener, err := guestvsock.Listen(pooloutbound.ContainerPort)
	if err != nil {
		return err
	}
	defer containerListener.Close()
	container := r
	if r.HTTP != "" || r.HTTPS != "" {
		address, err := waitDockerBridge(ctx)
		if err != nil {
			return err
		}
		for index, raw := range []string{r.HTTP, r.HTTPS} {
			if raw == "" {
				continue
			}
			target := net.JoinHostPort(address, ports[index])
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", target)
			if err != nil {
				return err
			}
			scheme := []string{"http", "https"}[index]
			go func() { _ = pooloutbound.ServeGuestRelay(ctx, listener, dial, scheme) }()
			if index == 0 {
				container.HTTP = pooloutbound.RelayEndpoint(raw, target)
			} else {
				container.HTTPS = pooloutbound.RelayEndpoint(raw, target)
			}
		}
	}
	go func() { <-ctx.Done(); _ = containerListener.Close() }()
	for {
		conn, err := containerListener.Accept()
		if err != nil {
			return err
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := json.NewEncoder(conn).Encode(pooloutbound.GuestDelivery{Runtime: container, Probes: probes}); err != nil {
			conn.Close()
			continue
		}
		conn.Close()
	}
}

func waitDockerBridge(ctx context.Context) (string, error) {
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		iface, err := net.InterfaceByName("docker0")
		if err == nil {
			addresses, _ := iface.Addrs()
			for _, address := range addresses {
				ip, _, err := net.ParseCIDR(address.String())
				if err == nil && ip.To4() != nil {
					return strings.TrimSpace(ip.String()), nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", fmt.Errorf("docker bridge did not become ready for pool outbound routing")
		case <-tick.C:
		}
	}
}
