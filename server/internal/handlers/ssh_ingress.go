package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// GetSSHIngress serves the SSH discovery document (ADR 0024, ADR 0057): the
// host key to pin, resolved once at startup. There is nothing else to serve —
// SSH reaches this server over the transport the API already answers on, so
// there is no address a client could dial instead and no way to turn it off.
//
// It is a public path (auth.IsPublicPath): a host public key is not a
// credential, and `disco box ssh-config` has to read it before any other
// credential exists.
func (h *Handler) GetSSHIngress(context.Context) (serverapi.GetSSHIngressRes, error) {
	return &apimodel.SSHIngress{HostKey: h.services.SSH.HostKey}, nil
}
