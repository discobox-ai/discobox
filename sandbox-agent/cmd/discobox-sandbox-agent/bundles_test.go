package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExportProxyEnvTakesTheBootstrapsProxyTrustEnvAndNothingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"env":{"HTTPS_PROXY":"http://127.0.0.1:3128","SSL_CERT_FILE":"/etc/discobox/proxy/mitm-ca.crt","GH_TOKEN":"discobox-sentinel-1"},"proxyEnvs":["HTTPS_PROXY","SSL_CERT_FILE"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "")
	os.Unsetenv("HTTPS_PROXY")
	t.Setenv("SSL_CERT_FILE", "/already/set")
	t.Setenv("GH_TOKEN", "")
	os.Unsetenv("GH_TOKEN")
	if err := exportProxyEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != "http://127.0.0.1:3128" {
		t.Fatalf("HTTPS_PROXY = %q", got)
	}
	if got := os.Getenv("SSL_CERT_FILE"); got != "/already/set" {
		t.Fatalf("SSL_CERT_FILE = %q, want the agent's own left as it was", got)
	}
	if _, set := os.LookupEnv("GH_TOKEN"); set {
		t.Fatal("a variable that is not proxy-trust env was exported")
	}
}
