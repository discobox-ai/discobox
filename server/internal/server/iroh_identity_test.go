package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
)

// Every server has a peer ID, whatever it listens on (ADR 0114): one that
// listens only on http loads the identity an iroh listener would use, keeps it
// across restarts, and sets up nothing for iroh.
func TestConfigureIrohGivesEveryServerAPeerID(t *testing.T) {
	dataDir := t.TempDir()
	listen := []string{"http://127.0.0.1:0"}

	admission, id, watch, err := configureIroh(t.Context(), dataDir, listen, nil, "")
	if err != nil {
		t.Fatalf("configureIroh() error = %v", err)
	}
	if id == (endpoint.IrohID{}) {
		t.Fatal("a server with no iroh endpoint has no peer ID")
	}
	if admission != nil || watch != nil {
		t.Fatal("a server with no iroh endpoint set one up")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "iroh_endpoint_key")); err != nil {
		t.Fatalf("the identity key was not written: %v", err)
	}

	_, again, _, err := configureIroh(t.Context(), dataDir, listen, nil, "")
	if err != nil {
		t.Fatalf("configureIroh() again error = %v", err)
	}
	if again != id {
		t.Fatalf("peer ID after a restart = %s, want %s", again, id)
	}
}
