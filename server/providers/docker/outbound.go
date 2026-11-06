package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

func (d *LocalDriver) PoolOutbound(_ context.Context, _ string, want pooloutbound.Resolved) (pooloutbound.Delivery, error) {
	runtime := want
	for _, endpoint := range []*string{&runtime.HTTP, &runtime.HTTPS} {
		if *endpoint == "" {
			continue
		}
		u, _ := url.Parse(*endpoint)
		if u.Hostname() != pooloutbound.LogicalForwarderHost {
			continue
		}
		if dockerworker.DaemonLocalityForHost(d.DaemonHost()) != dockerworker.DaemonOnThisMachine {
			return pooloutbound.Delivery{}, errors.New("enclosing sandbox forwarder cannot reach a remote Docker daemon; configure a proxy reachable from that daemon's containers")
		}
		data, err := os.ReadFile("/run/discobox/proxy/nested-forwarder.json")
		if err != nil {
			return pooloutbound.Delivery{}, errors.New("enclosing Docker proxy bridge is not ready")
		}
		var bridge struct {
			ListenAddress string `json:"listenAddress"`
		}
		if json.Unmarshal(data, &bridge) != nil || bridge.ListenAddress == "" {
			return pooloutbound.Delivery{}, errors.New("enclosing Docker proxy bridge has no listener")
		}
		u.Host = bridge.ListenAddress
		*endpoint = u.String()
	}
	return pooloutbound.Delivery{Runtime: runtime, AppliedRevision: want.Revision()}, nil
}
