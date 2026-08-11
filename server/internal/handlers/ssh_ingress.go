package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// GetSSHIngress serves the SSH endpoint discovery document (ADR 0024): the
// address clients should dial and the host key to pin, resolved once at
// startup. It answers when SSH is disabled too — the ingress is opt-in, so
// "not enabled here" is an ordinary answer a client should be able to render,
// not a 404 it cannot distinguish from an unknown route.
//
// It is a public path (auth.IsPublicPath): neither field is a credential, and
// `disco box ssh-config` has to be able to read both before any other
// credential exists.
func (h *Handler) GetSSHIngress(context.Context) (serverapi.GetSSHIngressRes, error) {
	ingress := h.services.SSH
	body := &apimodel.SSHIngress{Enabled: ingress.Enabled}
	if ingress.Enabled {
		body.SetAddress(serverapi.NewOptString(ingress.Address))
		body.SetHostKey(serverapi.NewOptString(ingress.HostKey))
	}
	return body, nil
}
