package dockerworker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	relay "github.com/discobox-ai/discobox/server/providers/guestrelay"
)

// hostOutboundBootDir holds what belongs to this boot alone: the install log,
// result markers, /etc overlays and the private settings the install reads.
// It is tmpfs and may be noexec, so nothing here is executed directly.
const hostOutboundBootDir = "/run/discobox-host-outbound"

// hostOutboundProgramName is the directory, under the engine's host state
// root, that holds the relay the VM host executes. The state root is the one
// directory every VM backend guarantees writable and executable.
const hostOutboundProgramName = "host-outbound"

// VMOutbound describes both consumers of an owned VM's upstream. The driver
// owns the raw VSOCK relay; the engine places everything the guest needs over
// its existing Docker connection. No guest image helper is assumed. A direct
// boot configuration has nothing to install on the host, so it has no Host.
func VMOutbound(ctx context.Context, cli *client.Client, boot, want pooloutbound.Resolved) (pooloutbound.Delivery, error) {
	if boot.Direct() {
		return pooloutbound.Delivery{Runtime: boot, AppliedRevision: boot.Revision(), RestartRequired: boot.Revision() != want.Revision()}, nil
	}
	network, err := cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	bridge := ""
	for _, config := range network.Network.IPAM.Config {
		if config.Gateway.Is4() {
			bridge = config.Gateway.String()
			break
		}
	}
	if bridge == "" {
		return pooloutbound.Delivery{}, fmt.Errorf("VM Docker bridge has no IPv4 gateway")
	}
	host, runtime := boot, boot
	host.HTTP = pooloutbound.RelayEndpoint(boot.HTTP, "127.0.0.1:17008")
	host.HTTPS = pooloutbound.RelayEndpoint(boot.HTTPS, "127.0.0.1:17009")
	runtime.HTTP = pooloutbound.RelayEndpoint(boot.HTTP, net.JoinHostPort(bridge, "17008"))
	runtime.HTTPS = pooloutbound.RelayEndpoint(boot.HTTPS, net.JoinHostPort(bridge, "17009"))
	return pooloutbound.Delivery{Host: &host, Runtime: runtime, AppliedRevision: boot.Revision(), RestartRequired: boot.Revision() != want.Revision()}, nil
}

// prepareHostOutbound imports a tiny command image from the server's embedded
// static helper. It requires neither a registry pull nor a preinstalled agent,
// including in development before the first bundle has been built.
func (e *Engine) prepareHostOutbound(ctx context.Context, cli *client.Client, poolID string, delivery *pooloutbound.Delivery) error {
	if delivery.Host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	binary, err := relay.Binary()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(binary)
	image := "discobox-host-command:" + hex.EncodeToString(digest[:])
	if _, err := cli.ImageInspect(ctx, image); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return err
		}
		var archive bytes.Buffer
		if err := hostArchive(&archive, map[string][]byte{"relay": binary}, "relay"); err != nil {
			return err
		}
		imported, err := cli.ImageImport(ctx, client.ImageImportSource{Source: &archive, SourceName: "-"}, image, client.ImageImportOptions{})
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(imported)
		for {
			var message struct {
				Error string `json:"error"`
			}
			err := decoder.Decode(&message)
			if err == io.EOF {
				break
			}
			if err != nil {
				imported.Close()
				return err
			}
			if message.Error != "" {
				imported.Close()
				return fmt.Errorf("import host command: %s", message.Error)
			}
		}
		imported.Close()
	}
	name := "discobox-host-command-" + poolID
	existing, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err == nil && existing.Container.Config.Image != image {
		if _, err := cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil {
			return err
		}
		_, err = cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	}
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			return err
		}
		_, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name,
			Config:     &container.Config{Image: image, Entrypoint: []string{"/relay"}, Cmd: []string{"--host-command", "--", "/bin/sleep", "infinity"}, Env: []string{"DISCOBOX_PROXY_ENVIRONMENT=managed"}},
			HostConfig: &container.HostConfig{Privileged: true, PidMode: "host", NetworkMode: "host", SecurityOpt: []string{"apparmor=unconfined", "seccomp=unconfined", "label=disable"}, Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/", Target: "/host"}}},
		})
		if err != nil {
			return err
		}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(cleanup, name, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return err
	}
	guest := provisioner.ContainerGuest{Client: cli, Container: name, Env: []string{"DISCOBOX_PROXY_ENVIRONMENT=managed"}}
	revision := delivery.Host.Revision()
	programDir := e.hostOutboundProgramDir()
	readResult := func(suffix string) ([]byte, error) {
		archive, err := guest.CopyFrom(ctx, "/host"+hostOutboundBootDir+"/"+revision+suffix)
		if err != nil {
			return nil, err
		}
		defer archive.Close()
		tr := tar.NewReader(archive)
		if _, err := tr.Next(); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(tr, 64<<10))
	}
	if result, err := readResult(".done"); err == nil {
		var probes pooloutbound.Probes
		if err := json.Unmarshal(result, &probes); err == nil {
			delivery.GuestProbes = &probes
			return nil
		}
	}
	files, err := delivery.Host.Files()
	if err != nil {
		return err
	}
	staged := map[string][]byte{"install.sh": []byte(hostOutboundInstall)}
	for name, data := range files {
		staged["root"+name] = data
	}
	bridge := ""
	network, err := cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	for _, config := range network.Network.IPAM.Config {
		if config.Gateway.Is4() {
			bridge = config.Gateway.String()
			break
		}
	}
	staged["relay.json"], err = json.Marshal(pooloutbound.RelaySettings{HTTP: delivery.Host.HTTP != "", HTTPS: delivery.Host.HTTPS != "", Bridge: bridge})
	if err != nil {
		return err
	}
	staged["revision"] = []byte(revision)
	var bootArchive, programArchive bytes.Buffer
	if err := hostArchive(&bootArchive, staged, ""); err != nil {
		return err
	}
	if err := hostArchive(&programArchive, map[string][]byte{"relay": binary}, "relay"); err != nil {
		return err
	}
	var stderr bytes.Buffer
	command := []string{"/relay", "--host-command", "--", "/bin/sh", "-ec", `install -d -m 700 "$1" "$2"; rm -f "$1/$3.failed"`, "sh", hostOutboundBootDir, programDir, revision}
	if code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: command}, nil, io.Discard, &stderr); err != nil {
		return fmt.Errorf("prepare host directories: %w: %s", err, &stderr)
	} else if code != 0 {
		return fmt.Errorf("prepare host directories exited %d: %s", code, &stderr)
	}
	if err := guest.CopyTo(ctx, "/host"+hostOutboundBootDir, &bootArchive); err != nil {
		return err
	}
	if err := guest.CopyTo(ctx, "/host"+programDir, &programArchive); err != nil {
		return err
	}
	// The script is read by sh, never executed, so noexec on the boot directory
	// is harmless; the relay it starts is the only file executed.
	command = []string{"/relay", "--host-command", "--", "systemd-run", "--unit=discobox-host-outbound-install", "--collect", "/bin/sh", hostOutboundBootDir + "/install.sh", programDir}
	if code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: command}, nil, io.Discard, &stderr); err == nil && code != 0 {
		return fmt.Errorf("start host installation exited %d: %s", code, &stderr)
	}
	// A transport error can mean Docker restarted after systemd accepted the
	// job. Read the durable result before treating that lost response as failure.
	// Restarting Docker may close this exec transport and stop the command
	// container. systemd owns installation; CopyFrom reads the mounted host state.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if result, err := readResult(".done"); err == nil {
			var probes pooloutbound.Probes
			if err := json.Unmarshal(result, &probes); err != nil {
				return fmt.Errorf("host outbound installation failed: %s", result)
			}
			delivery.GuestProbes = &probes
			return nil
		}
		if _, err := readResult(".failed"); err == nil {
			return fmt.Errorf("host outbound installation failed; see %s/install.log on the pool host", hostOutboundBootDir)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for host outbound installation: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// hostOutboundProgramDir is the VM host's view of this engine's state root,
// where the relay is placed: /var/lib/discobox on vz and libkrun, and the
// relocated root on a backend that sets HostStateRoot.
func (e *Engine) hostOutboundProgramDir() string {
	return path.Join(layout.Container().HostMapping(e.cfg.HostStateRoot).HostRoot(), hostOutboundProgramName)
}

func hostArchive(writer io.Writer, files map[string][]byte, executable string) error {
	tw := tar.NewWriter(writer)
	for name, data := range files {
		mode := int64(0600)
		if name == executable {
			mode = 0700
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// hostOutboundInstall runs as "sh install.sh PROGRAM_DIR". PROGRAM_DIR holds
// the relay on storage that permits execution; $root may be noexec.
const hostOutboundInstall = `set -eu
root=/run/discobox-host-outbound
exec >"$root/install.log" 2>&1
revision=$(cat "$root/revision")
trap 'echo "installation failed; see /run/discobox-host-outbound/install.log" > "$root/$revision.failed"' EXIT
program=${1:?missing program directory}
# The root disk is shared read-only. All writes belong to this boot alone.
for target in /etc /usr/local/share/ca-certificates; do
 overlay=$root/overlay$(echo "$target" | tr / _)
 if ! mountpoint -q "$target"; then
  mkdir -p "$overlay/upper" "$overlay/work"
  mount -t overlay overlay -o "lowerdir=$target,upperdir=$overlay/upper,workdir=$overlay/work" "$target"
 fi
done
cp -a "$root/root/etc/." /etc/
export DISCOBOX_POOL_OUTBOUND_REVISION=$revision
sh /etc/discobox/install-pool-outbound
cat > /etc/systemd/system/discobox-outbound.service <<UNIT
[Unit]
Description=Pool outbound byte forwarding
[Service]
Type=notify
ExecStart=$program/relay --outbound $root/relay.json
Restart=on-failure
RestartSec=1
UNIT
systemctl daemon-reload
systemctl restart discobox-outbound.service
systemctl restart docker.service
"$program/relay" --outbound-probe > "$root/result"
mv "$root/result" "$root/$revision.done"
trap - EXIT
`
