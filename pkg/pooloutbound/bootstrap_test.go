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

func TestInheritedForwarderRecognitionRequiresDeliveredMatchingEndpoint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sandbox.json")
	raw, err := json.Marshal(enclosingRuntime{SandboxID: "sbx_outer", ProxyEnvs: []string{"HTTP_PROXY", "HTTPS_PROXY"}, Env: map[string]string{"HTTP_PROXY": "http://127.0.0.1:17008", "HTTPS_PROXY": "http://127.0.0.1:17008"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		input, want Resolved
	}{
		{name: "known endpoint with empty direct HTTPS", input: Resolved{HTTP: "http://127.0.0.1:17008"}, want: Resolved{HTTP: "http://" + LogicalForwarderHost}},
		{name: "independent external HTTPS", input: Resolved{HTTP: "http://127.0.0.1:17008", HTTPS: "http://enterprise.example:3129"}, want: Resolved{HTTP: "http://" + LogicalForwarderHost, HTTPS: "http://enterprise.example:3129"}},
		{name: "unrelated localhost proxy", input: Resolved{HTTP: "http://127.0.0.1:9999"}, want: Resolved{HTTP: "http://127.0.0.1:9999"}},
		{name: "explicit direct", input: Resolved{}, want: Resolved{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := recognizeInheritedForwarder(test.input, file)
			if err != nil || got != test.want {
				t.Fatalf("recognized routing=%+v error=%v, want %+v", got, err, test.want)
			}
		})
	}
	if err := os.WriteFile(file, []byte(`{"sandboxId":"sbx_outer","proxyEnvs":["UNRELATED"],"env":{"HTTP_PROXY":"http://127.0.0.1:17008"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	original := Resolved{HTTP: "http://127.0.0.1:17008"}
	if got, err := recognizeInheritedForwarder(original, file); err != nil || got != original {
		t.Fatalf("undeclared endpoint qualified: %+v %v", got, err)
	}
}
