package proxyagent

import (
	"context"
	"fmt"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/proxy"
)

func (r *secretResolver) Authorize(ctx context.Context, req proxy.SecretAuthorizeRequest) error {
	ctx, cancel := context.WithTimeout(ctx, judge.Timeout+15*time.Second)
	defer cancel()
	var uses []activation
	for _, sentinel := range req.Sentinels {
		key := proxy.SecretResolveRequest{ClientID: req.ClientID, Sentinel: sentinel, Host: req.Host}
		if record, ok := r.activation(key); ok {
			uses = append(uses, record)
		} else if r.isEphemeralCandidate(key) {
			return proxy.ErrSecretResolveDenied
		}
	}
	if len(uses) == 0 {
		return nil
	} // ordinary harness credentials have no use scope
	broker := &controlPlaneCredentials{contextPath: r.contextPath, client: r.client}
	docs, err := broker.list(ctx, req.ClientID)
	if err != nil {
		return err
	}
	evidence, evidenceErr := req.Evidence(ctx)
	purposes := make([]string, len(uses))
	hosts := make([]string, len(uses))
	for index, record := range uses {
		var selected *credentialDoc
		var purpose string
		for i := range docs {
			if docs[i].Sentinel == record.Stable {
				for _, use := range docs[i].Uses {
					if use.UseID == record.UseID {
						selected = &docs[i]
						purpose = use.Description
					}
				}
			}
		}
		if selected == nil {
			return proxy.ErrSecretResolveDenied
		}
		purposes[index], hosts[index] = purpose, selected.Host
		job := judge.Job{Kind: "request", Purpose: purpose, Host: selected.Host, Credential: selected.Name, Command: record.Command, Evidence: evidence}
		var verdict judge.Verdict
		judgeErr := evidenceErr
		if judgeErr == nil {
			verdict, judgeErr = callPoolJudge(ctx, job)
		}
		if judgeErr != nil {
			verdict.Allow = false
			verdict.Reason = "request could not be judged"
		}
		if err := broker.recordTrustedVerdict(ctx, req.ClientID, record.UseID, record.Command, "request", req.RequestID, verdict); err != nil {
			return err
		}
		if judgeErr != nil {
			return fmt.Errorf("credential request denied: %w", judgeErr)
		}
		if !verdict.Allow {
			return fmt.Errorf("credential request denied: %s", verdict.Reason)
		}
		current, err := broker.list(ctx, req.ClientID)
		if err != nil {
			return err
		}
		if !liveCredentialUse(current, record.Stable, record.UseID, purpose, selected.Host) {
			return proxy.ErrSecretResolveDenied
		}
		if !record.live(time.Now()) {
			return proxy.ErrSecretResolveDenied
		}
	}
	current, err := broker.list(ctx, req.ClientID)
	if err != nil {
		return err
	}
	for i, record := range uses {
		if !record.live(time.Now()) || !liveCredentialUse(current, record.Stable, record.UseID, purposes[i], hosts[i]) {
			return proxy.ErrSecretResolveDenied
		}
	}
	return nil
}
