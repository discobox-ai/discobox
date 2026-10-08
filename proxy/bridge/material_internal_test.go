package bridge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
)

// A certificate that runs out while it is held is refused from that moment,
// not presented until something replaces it; one not yet valid is refused
// until it is.
func TestMaterialRefusesACertificateOutsideItsValidity(t *testing.T) {
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         filepath.Join(t.TempDir(), "certs"),
		ServerHosts: []string{"discobox-pool-proxy"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := prepared.Clients["sandbox-1"]
	material, err := LoadMaterial(client.MTLSCAPath, client.ClientCertPath, client.ClientKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := material.TLSConfig("discobox-pool-proxy"); err != nil {
		t.Fatalf("TLSConfig inside the validity: %v", err)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"after", client.ExpiresAt.Add(time.Second), "expired"},
		{"before", client.GeneratedAt.Add(-time.Second), "not valid until"},
	} {
		material.now = func() time.Time { return tc.at }
		if _, err := material.TLSConfig("discobox-pool-proxy"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s the validity: TLSConfig error = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}
