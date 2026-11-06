package pooloutbound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNestedBootstrapRecordsLogicalExplicitIntentFromDeliveredMaterial(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://ambient.invalid:3128")
	file := filepath.Join(t.TempDir(), "sandbox.json")
	write := func(id string, names []string) {
		raw, err := json.Marshal(map[string]any{"sandboxId": id, "proxyEnvs": names, "env": map[string]string{"HTTP_PROXY": "http://127.0.0.1:17008", "HTTPS_PROXY": "http://127.0.0.1:17008"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("sbx_enclosing", []string{"HTTP_PROXY", "HTTPS_PROXY"})
	intent, err := NestedBootstrap(file)
	if err != nil {
		t.Fatal(err)
	}
	if intent == nil || intent.Mode != "explicit" || intent.HTTP != "http://"+LogicalForwarderHost || intent.HTTPS != intent.HTTP {
		t.Fatalf("bootstrap intent: %+v", intent)
	}
	write("sbx_enclosing", nil)
	if got, err := NestedBootstrap(file); err != nil || got != nil {
		t.Fatalf("ambient variables qualified as delivered proxy material: %+v %v", got, err)
	}
	write("", []string{"HTTP_PROXY", "HTTPS_PROXY"})
	if got, err := NestedBootstrap(file); err != nil || got != nil {
		t.Fatalf("unrecognized runtime qualified: %+v %v", got, err)
	}
}
