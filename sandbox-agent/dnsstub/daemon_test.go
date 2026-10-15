package dnsstub

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// TestNestedDockerAsksTheStub holds the image's daemon.json to the stub's
// address: the nested Docker hands its containers that one name server, and
// on its default bridge there is no embedded resolver to forward for them.
func TestNestedDockerAsksTheStub(t *testing.T) {
	data, err := os.ReadFile("../image/daemon.json")
	if err != nil {
		t.Fatal(err)
	}
	var daemon struct {
		DNS []string `json:"dns"`
	}
	if err := json.Unmarshal(data, &daemon); err != nil {
		t.Fatal(err)
	}
	if want := []string{sandboxconfig.SandboxDNSAddress}; !slices.Equal(daemon.DNS, want) {
		t.Errorf("daemon.json dns = %q, want %q", daemon.DNS, want)
	}
}
