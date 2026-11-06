package pools

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"sort"

	apimodel "github.com/discobox-ai/discobox/api/model"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *ControlPlane) ResolveOutbound(ctx context.Context, pool *model.Pool) error {
	intent, err := s.store.OpenPoolProxy(ctx, pool)
	if err != nil {
		return err
	}
	resolved, source := s.OutboundDefaults.Select(pool.ID)
	mode := s.OutboundDefaults.GlobalMode
	if m, ok := s.OutboundDefaults.Modes[pool.ID]; ok {
		mode = m
	}
	if mode == "" {
		mode = "direct"
	}
	if intent != nil {
		resolved, err = pooloutbound.Resolve(*intent, os.LookupEnv)
		if err != nil {
			return err
		}
		source = "api"
		if pool.ProxyBootstrap {
			source = "bootstrap"
		}
		mode = intent.Mode
		if mode == "" {
			mode = "direct"
		}
		redacted := *intent
		redacted.HTTP = pooloutbound.Redact(intent.HTTP)
		redacted.HTTPS = pooloutbound.Redact(intent.HTTPS)
		pool.Proxy = &redacted
	}
	anchors, _ := pooloutbound.Anchors(resolved.CAPEM)
	fps := make([]string, 0, len(anchors))
	for fp := range anchors {
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	_, yaml := s.OutboundDefaults.ByPool[pool.ID]
	pool.Outbound = resolved
	probes := map[string]pooloutbound.Probes{}
	_ = json.Unmarshal(pool.OutboundProbes, &probes)
	untested := pooloutbound.Probes{HTTP: pooloutbound.Probe{State: "untested"}, HTTPS: pooloutbound.Probe{State: "untested"}}
	for _, hop := range []string{"server", "guest", "pool"} {
		if _, ok := probes[hop]; !ok {
			probes[hop] = untested
		}
	}
	pool.OutboundStatus = &model.PoolOutboundStatus{Probes: probes, Source: source, Mode: mode, HTTP: pooloutbound.Redact(resolved.HTTP), HTTPS: pooloutbound.Redact(resolved.HTTPS), CAFingerprints: fps, DesiredRevision: resolved.Revision(), AppliedRevision: pool.OutboundAppliedRevision, RestartRequired: pool.OutboundRestartRequired, ShadowsYAML: intent != nil && yaml}
	return nil
}

// proxyWrite preserves credentials on a redacted round trip. Removal is explicit.
func proxyWrite(input apimodel.PoolProxy, old *pooloutbound.Intent) (*pooloutbound.Intent, error) {
	i := &pooloutbound.Intent{Mode: string(input.Mode.Or("direct")), HTTP: input.HTTP.Or(""), HTTPS: input.HTTPS.Or(""), NoProxy: input.NoProxy.Or(""), CAPEM: input.CaPem.Or("")}
	if i.Mode != "explicit" && (input.HTTP.IsSet() || input.HTTPS.IsSet() || input.NoProxy.IsSet() || input.CaPem.IsSet()) {
		return nil, errors.New("proxy payload requires mode explicit")
	}
	merge := func(raw, previous string, removeCredentials bool) (string, error) {
		if err := pooloutbound.ValidateEndpoint(raw); err != nil {
			return "", err
		}
		if raw == "" {
			return "", nil
		}
		u, _ := url.Parse(raw)
		if removeCredentials && u.User != nil {
			return "", errors.New("proxy credential replacement and removal cannot be combined")
		}
		if !removeCredentials && u.User == nil && previous != "" {
			prior, _ := url.Parse(previous)
			if prior != nil {
				u.User = prior.User
			}
		}
		return u.String(), nil
	}
	prev := pooloutbound.Intent{}
	if old != nil {
		prev = *old
	}
	var err error
	i.HTTP, err = merge(i.HTTP, prev.HTTP, input.ClearHttpCredentials.Or(false))
	if err != nil {
		return nil, err
	}
	i.HTTPS, err = merge(i.HTTPS, prev.HTTPS, input.ClearHttpsCredentials.Or(false))
	if err != nil {
		return nil, err
	}
	if _, err := pooloutbound.Resolve(*i, os.LookupEnv); err != nil {
		return nil, err
	}
	return i, nil
}
