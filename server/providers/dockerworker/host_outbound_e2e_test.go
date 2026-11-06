package dockerworker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	relay "github.com/discobox-ai/discobox/server/providers/guestrelay"
)

// This deliberately restarts Docker on an already-provisioned disposable VM.
// Never point it at an operator's daemon: the fixture changes host systemd.
func TestHostOutboundRetriesFailedRevisionE2E(t *testing.T) {
	host := os.Getenv("DISCOBOX_TEST_OUTBOUND_DOCKER")
	if host == "" {
		t.Skip("set DISCOBOX_TEST_OUTBOUND_DOCKER to a disposable provisioned VM's Docker socket")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cli, err := NewDockerClientForHost(host)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	binary, err := relay.Binary()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "discobox-host-command:" + hex.EncodeToString(sum[:]), Entrypoint: []string{"/relay"}, Cmd: []string{"--host-command", "--", "/bin/sleep", "infinity"}},
		HostConfig: &container.HostConfig{Privileged: true, PidMode: "host", NetworkMode: "host", Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/", Target: "/host"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	guest := provisioner.ContainerGuest{Client: cli, Container: created.ID}
	read := func(path string, value any) {
		t.Helper()
		archive, err := guest.CopyFrom(ctx, "/host"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		tr := tar.NewReader(archive)
		if _, err := tr.Next(); err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(tr).Decode(value); err != nil {
			t.Fatal(err)
		}
	}
	var resolved pooloutbound.Resolved
	read(pooloutbound.RuntimeFile, &resolved)
	delivery, err := VMOutbound(ctx, cli, resolved, resolved)
	if err != nil {
		t.Fatal(err)
	}
	revision := delivery.Host.Revision()
	run := func(script string) {
		t.Helper()
		var stderr strings.Builder
		code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: []string{"/relay", "--host-command", "--", "/bin/sh", "-ec", script}}, nil, io.Discard, &stderr)
		if err != nil || code != 0 {
			t.Fatalf("fixture command: %v (exit %d): %s", err, code, &stderr)
		}
	}
	run(`mkdir -p /etc/systemd/system/discobox-outbound.service.d
cat > /etc/systemd/system/discobox-outbound.service.d/99-retry-test.conf <<'UNIT'
[Service]
ExecStartPre=/bin/sh -ec 'test -e /run/discobox-outbound-retry-once || { touch /run/discobox-outbound-retry-once; exit 1; }'
UNIT
rm -f /run/discobox-outbound-retry-once ` + hostOutboundDir + "/" + revision + `.done
systemctl daemon-reload`)
	defer func() {
		_, _ = cli.ContainerStart(context.WithoutCancel(ctx), created.ID, client.ContainerStartOptions{})
		run(`rm -f /etc/systemd/system/discobox-outbound.service.d/99-retry-test.conf /run/discobox-outbound-retry-once; systemctl daemon-reload`)
	}()
	engine := &Engine{}
	if err := engine.prepareHostOutbound(ctx, cli, "retry-test", &delivery); err == nil || !strings.Contains(err.Error(), "installation failed") {
		t.Fatalf("first install = %v, want injected failure", err)
	}
	// No marker is removed or configuration changed between these attempts.
	if err := engine.prepareHostOutbound(ctx, cli, "retry-test", &delivery); err != nil {
		t.Fatalf("same-revision retry: %v", err)
	}
	if delivery.GuestProbes == nil {
		t.Fatal("successful retry returned no probes")
	}
	t.Log("injected install failure recovered on the same revision without editing its result markers")
}
