package digitalocean

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker/sshdocker"
)

// PoolOutbound stages boot intent without restarting an operator's workloads,
// and reads the configuration actually applied on the machine over private SSH.
func (d *Driver) PoolOutbound(ctx context.Context, poolID string, want pooloutbound.Resolved) (pooloutbound.Delivery, error) {
	if err := want.RejectLocalEndpoints(); err != nil {
		return pooloutbound.Delivery{}, err
	}
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	connection, err := d.ssh.Dial(ctx, sshdocker.Target{Host: publicIPv4(droplet.Networks)})
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	session, err := connection.NewSession()
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	raw, err := session.Output("if test -f /etc/discobox/pool-outbound/current/runtime.json; then cat /etc/discobox/pool-outbound/current/runtime.json; else printf '{}'; fi")
	session.Close()
	if err != nil {
		return pooloutbound.Delivery{}, fmt.Errorf("%w: cannot read machine outbound revision", sandbox.ErrPoolConfigApply)
	}
	var applied pooloutbound.Resolved
	if json.Unmarshal(raw, &applied) != nil {
		return pooloutbound.Delivery{}, errors.New("invalid machine outbound boot material")
	}
	if applied.Revision() == want.Revision() {
		return pooloutbound.Delivery{Runtime: applied, AppliedRevision: applied.Revision()}, nil
	}
	files, err := want.Files()
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	files["/etc/systemd/system/discobox-outbound-boot.service"] = []byte("[Unit]\nDescription=Pool outbound boot configuration\nBefore=docker.service\nAfter=local-fs.target\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nEnvironment=" + pooloutbound.RevisionEnv + "=" + want.Revision() + "\nExecStart=/bin/sh " + pooloutbound.InstallFile + "\n\n[Install]\nWantedBy=multi-user.target\n")
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: strings.TrimPrefix(name, "/"), Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return pooloutbound.Delivery{}, err
		}
		if _, err := writer.Write(data); err != nil {
			return pooloutbound.Delivery{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return pooloutbound.Delivery{}, err
	}
	session, err = connection.NewSession()
	if err != nil {
		return pooloutbound.Delivery{}, err
	}
	defer session.Close()
	session.Stdin = &archive
	if err := session.Run("tar -xpf - -C / && systemctl daemon-reload && systemctl enable discobox-outbound-boot.service"); err != nil {
		return pooloutbound.Delivery{}, fmt.Errorf("%w: cannot stage machine outbound configuration for its next restart", sandbox.ErrPoolConfigApply)
	}
	return pooloutbound.Delivery{Runtime: applied, AppliedRevision: applied.Revision(), RestartRequired: true}, nil
}
