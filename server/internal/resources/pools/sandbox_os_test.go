package pools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/platform"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// A pool names its sandboxes' OS when it is created (ADR 26-11-11-963 §1):
// linux by default and on every provider, stored named; another OS only where
// the provider type's validator says its instance boots it.
func TestCreatePoolNamesItsSandboxOS(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()
	svc.providers.RegisterPoolOSValidator(sizedType, func(_ json.RawMessage, os string) error {
		if os == platform.Darwin {
			return nil
		}
		return errors.New("boots only darwin")
	})

	pool, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "default-os", ProviderInstanceId: "prov-unsized"})
	if err != nil {
		t.Fatal(err)
	}
	if pool.SandboxOS != platform.Linux {
		t.Fatalf("a pool that names no OS stored %q, want linux named", pool.SandboxOS)
	}

	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "darwin-on-shared", ProviderInstanceId: "prov-unsized",
		SandboxOs: serverapi.NewOptCreatePoolBodySandboxOs(serverapi.CreatePoolBodySandboxOsDarwin),
	})
	requireBadRequest(t, err, "Shared", "only linux")

	pool, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "darwin", ProviderInstanceId: "prov-sized",
		SandboxOs: serverapi.NewOptCreatePoolBodySandboxOs(serverapi.CreatePoolBodySandboxOsDarwin),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pool.SandboxOS != platform.Darwin {
		t.Fatalf("sandbox OS = %q, want darwin", pool.SandboxOS)
	}
	// The validator decides linux too, for a type that registers one.
	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "linux-on-sized", ProviderInstanceId: "prov-sized"})
	requireBadRequest(t, err, "boots only darwin")

	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "windows", ProviderInstanceId: "prov-unsized",
		SandboxOs: serverapi.NewOptCreatePoolBodySandboxOs("windows"),
	})
	requireBadRequest(t, err, "windows")
}
