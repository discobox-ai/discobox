package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderResolverNameservers(t *testing.T) {
	record := ProviderResolver{Content: "# provider\nnameserver 169.254.53.53\nnameserver not-an-ip\nnameserver 10.40.0.1 # comment\nnameserver fe80::1%eth0\nsearch example.test\n"}
	if got := strings.Join(record.Nameservers(), ","); got != "10.40.0.1,fe80::1%eth0" {
		t.Fatal(got)
	}
}
func TestProviderDialFailsClosedOnBrokenRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	for _, content := range []string{"not json", `{"content":"nameserver 169.254.53.53\n"}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		conn, err := providerDialContext(path)(context.Background(), "tcp", "localhost:1")
		if err == nil {
			_ = conn.Close()
			t.Fatal("dial ignored broken record")
		}
		if !strings.Contains(err.Error(), "provider resolver") {
			t.Fatalf("fell through to system DNS: %v", err)
		}
	}
}
