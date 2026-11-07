package dockerworker

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
)

// The relay is the only file the VM host executes, so it must live under the
// engine's state root — the one directory every backend guarantees writable
// and executable — and never under /run, which a guest may mount noexec.
func TestHostOutboundRelayLivesUnderTheStateRoot(t *testing.T) {
	for _, tc := range []struct{ stateRoot, want string }{
		{"", "/var/lib/discobox/host-outbound"},
		{"/var/lib/docker/discobox", "/var/lib/docker/discobox/host-outbound"},
	} {
		e := &Engine{cfg: Config{HostStateRoot: tc.stateRoot}}
		if got := e.hostOutboundProgramDir(); got != tc.want {
			t.Fatalf("HostStateRoot %q: program dir = %q, want %q", tc.stateRoot, got, tc.want)
		}
	}
	for line := range strings.SplitSeq(hostOutboundInstall, "\n") {
		if strings.Contains(line, "/relay") && !strings.Contains(line, "$program/relay") {
			t.Fatalf("install script executes a relay outside the program directory: %q", line)
		}
	}
}

func TestDirectVMOutboundInstallsNothingOnTheHost(t *testing.T) {
	boot := pooloutbound.Resolved{NoProxy: "example.com"}
	delivery, err := VMOutbound(t.Context(), nil, boot, boot)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Host != nil {
		t.Fatalf("direct boot configuration delivered host material: %+v", delivery.Host)
	}
	if delivery.AppliedRevision != boot.Revision() || delivery.RestartRequired {
		t.Fatalf("delivery = %+v, want boot revision applied without restart", delivery)
	}
	if (pooloutbound.Resolved{HTTP: "http://proxy:3128"}).Direct() {
		t.Fatal("a proxied configuration reported direct")
	}
}
