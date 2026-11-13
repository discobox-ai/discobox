package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-faster/jx"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// driverCatalogItem is a disco-vm driver's provider type as the server lists
// it: its secret and fields under driver (ADR 26-11-11-080 §2).
func driverCatalogItem() apimodel.SandboxProviderCatalogItem {
	provider := apimodel.SandboxProviderCatalogItem{ID: "boxd", Name: "boxd"}
	provider.ConfigFields.SetTo([]apimodel.ProviderConfigField{
		{Key: "driver.apiKey", Label: "API Key", Type: "secret", Required: apiclientgen.NewOptBool(true)},
		{Key: "driver.region", Label: "Region", Type: "string", Immutable: apiclientgen.NewOptBool(true)},
		{Key: "driver.tokenUrl", Label: "Token URL", Type: "string", Advanced: apiclientgen.NewOptBool(true), Privileged: apiclientgen.NewOptBool(true), Default: apiclientgen.NewOptString("https://app.boxd.sh/api/v1/auth/token")},
		{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string"},
	})
	return provider
}

// A driver field is a --driver-<key> flag in kebab case, set inside the
// driver object, and its secret is --secret driver.<key>.
func TestADriversFieldsAreFlagsSetInsideDriver(t *testing.T) {
	provider := driverCatalogItem()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("bxd_key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewRootCommand()
	createCmd, _, err := cmd.Find([]string{"admin", "provider", "create"})
	if err != nil {
		t.Fatal(err)
	}
	opts, err := parseDynamicProviderCreateArgs(createCmd, []string{"--type", "boxd", "--driver-region", "fra", "--driver-token-url", "https://t.example.com/token", "--control-plane-url", "https://d.example.com", "--secret", "driver.apiKey=@" + keyFile}, provider)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	body, err := dynamicProviderCreateBody(opts, provider)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := string(body.GetConfig()); got != `{"controlPlaneUrl":"https://d.example.com","driver":{"apiKey":"bxd_key","region":"fra","tokenUrl":"https://t.example.com/token"}}` {
		t.Fatalf("config = %s", got)
	}
	opts, err = parseDynamicProviderCreateArgs(createCmd, []string{"--type", "boxd", "--driver-region", "fra"}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicProviderCreateBody(opts, provider); err == nil || !strings.Contains(err.Error(), "--secret driver.apiKey=@path") {
		t.Fatalf("a create without the key = %v, want it named", err)
	}

	// An update sets one field inside the driver object it read, keeping
	// the rest of it.
	updateCmd, _, err := cmd.Find([]string{"admin", "provider", "update"})
	if err != nil {
		t.Fatal(err)
	}
	current := &apimodel.SandboxProviderInstance{Config: jx.Raw(`{"driver":{"region":"fra"}}`)}
	update, err := parseDynamicProviderUpdateArgs(updateCmd, []string{"provider-1", "--driver-token-url", "https://t.example.com/token", "--secret", "driver.apiKey=@" + keyFile}, provider)
	if err != nil {
		t.Fatal(err)
	}
	updateBody, err := dynamicProviderUpdateBody(update, provider, current)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(updateBody.GetConfig()); got != `{"driver":{"apiKey":"bxd_key","region":"fra","tokenUrl":"https://t.example.com/token"}}` {
		t.Fatalf("update config = %s", got)
	}

	var help bytes.Buffer
	if err := writeProviderCreateHelp(&help, provider); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--driver-region", "--driver-token-url", "default: https://app.boxd.sh/api/v1/auth/token", "driver.apiKey"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help does not show %q:\n%s", want, help.String())
		}
	}
}
