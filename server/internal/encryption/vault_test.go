package encryption

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	vault "github.com/hashicorp/vault/api"
)

// A token read from a file is read again, so one rotated outside the server
// is taken up without a restart (ADR 26-11-02-902 §3).
func TestFollowTokenFileTakesUpARotatedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := vault.NewClient(vault.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	client.SetToken("first")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		followTokenFile(ctx, client, "file:"+path, "first", 5*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if err := os.WriteFile(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for client.Token() != "second" {
		if time.Now().After(deadline) {
			t.Fatalf("token = %q, want the rotated one", client.Token())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Windows cannot remove a file while the follower is reading it.
	cancel()
	<-done

	// A file that goes missing keeps the token last read.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	missingCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	followTokenFile(missingCtx, client, "file:"+path, "second", 5*time.Millisecond)
	if got := client.Token(); got != "second" {
		t.Fatalf("token after the file went = %q, want the last one read", got)
	}
}
