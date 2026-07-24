package nrica

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/nri/pkg/api"
)

func writeProxyEnvFile(t *testing.T, env map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy-env.json")
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal proxy env: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write proxy env: %v", err)
	}
	return path
}

func writeBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("cert"), 0o644); err != nil {
		t.Fatalf("write bundle %s: %v", name, err)
	}
}

func TestCreateContainerMountsAndEnv(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "debian.pem")
	writeBundle(t, dir, "alpine.pem")
	// rhel.pem intentionally not staged, to exercise the "not staged" skip.

	envPath := writeProxyEnvFile(t, map[string]string{
		"HTTP_PROXY":    "http://172.30.0.1:17008",
		"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt",
	})
	plugin, err := New(nil, envPath, dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	container := &api.Container{
		Name: "test",
		Env:  []string{"HTTP_PROXY=http://already-set"},
		Mounts: []*api.Mount{
			{Destination: "/etc/ssl/cert.pem", Source: "/user/provided", Type: "bind"},
		},
	}
	adjustment, updates, err := plugin.CreateContainer(context.Background(), &api.PodSandbox{}, container)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if updates != nil {
		t.Fatalf("expected no container updates, got %v", updates)
	}

	mountDests := map[string]bool{}
	for _, m := range adjustment.GetMounts() {
		mountDests[m.GetDestination()] = true
	}
	if mountDests["/etc/ssl/certs/ca-certificates.crt"] != true {
		t.Error("expected debian mount to be added")
	}
	if mountDests["/etc/ssl/cert.pem"] {
		t.Error("alpine mount should be skipped: container already sets it")
	}
	if mountDests["/etc/pki/tls/certs/ca-bundle.crt"] {
		t.Error("rhel mount should be skipped: bundle not staged")
	}

	envNames := map[string]string{}
	for _, kv := range adjustment.GetEnv() {
		envNames[kv.GetKey()] = kv.GetValue()
	}
	if _, ok := envNames["HTTP_PROXY"]; ok {
		t.Error("HTTP_PROXY should be skipped: container already sets it")
	}
	if got := envNames["SSL_CERT_FILE"]; got != "/etc/ssl/certs/ca-certificates.crt" {
		t.Errorf("SSL_CERT_FILE = %q, want the staged bundle path", got)
	}
}

func TestNewNoProxyEnvFile(t *testing.T) {
	dir := t.TempDir()
	plugin, err := New(nil, filepath.Join(dir, "missing.json"), dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(plugin.proxEnv) != 0 {
		t.Errorf("expected no proxy env when file is missing, got %v", plugin.proxEnv)
	}
}
