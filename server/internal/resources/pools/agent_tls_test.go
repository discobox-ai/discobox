package pools

import (
	"context"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A pool reaches its sandboxes' agents over TLS unless it opts out, at create
// or later; the setting is the pool's, and stored with it (ADR 26-10-29-481
// §3).
func TestAPoolServesItsAgentsOverTLSUnlessItOptsOut(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()

	byDefault, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "default", ProviderInstanceId: "prov-unsized"})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if byDefault.DisableAgentEndpointTLS {
		t.Fatal("a pool made with nothing said serves its agents over plain HTTP; want TLS")
	}
	optedOut, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "plain", ProviderInstanceId: "prov-unsized", DisableAgentEndpointTls: serverapi.NewOptBool(true)})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if !optedOut.DisableAgentEndpointTLS {
		t.Fatal("a pool made opting out of TLS serves its agents over TLS")
	}

	flipped, err := svc.UpdatePool(ctx, "project-1", optedOut.ID, services.UpdatePoolBody{DisableAgentEndpointTls: serverapi.NewOptBool(false)})
	if err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if flipped.DisableAgentEndpointTLS {
		t.Fatal("an update turning TLS back on left the pool on plain HTTP")
	}
	kept, err := svc.UpdatePool(ctx, "project-1", optedOut.ID, services.UpdatePoolBody{Name: serverapi.NewOptString("renamed")})
	if err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if kept.DisableAgentEndpointTLS {
		t.Fatal("an update that said nothing of TLS changed it")
	}
}
