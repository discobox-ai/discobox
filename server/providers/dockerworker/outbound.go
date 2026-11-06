package dockerworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func observePoolOutbound(ctx context.Context, cli *client.Client, containerID string, pool *model.Pool, previousRevision string) {
	observations := map[string]pooloutbound.Probes{}
	_ = json.Unmarshal(pool.OutboundProbes, &observations)
	if old, ok := observations["pool"]; ok && old.HTTP.State != "untested" && previousRevision == pool.OutboundAppliedRevision {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var out bytes.Buffer
	code, err := machine.Run(ctx, provisioner.ContainerGuest{Client: cli, Container: containerID, Env: pool.RuntimeOutbound.Environment()}, machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "outbound-probe"}}, nil, &out, io.Discard)
	var probes pooloutbound.Probes
	if err != nil || code != 0 || json.Unmarshal(out.Bytes(), &probes) != nil {
		probes = pooloutbound.Probes{HTTP: pooloutbound.Probe{State: "untested", Detail: "probe_execution_failed"}, HTTPS: pooloutbound.Probe{State: "untested", Detail: "probe_execution_failed"}}
	}
	observations["pool"] = probes
	if encoded, err := json.Marshal(observations); err == nil {
		pool.OutboundProbes = encoded
	}
}
