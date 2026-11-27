package proxyagent

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// A proxy starting beside another that is active, or one still draining
// after a handover it restarted in the middle of, writes the audit beside
// it; alone, it does not.
func TestAnotherProxyWritesWhileOneIsActiveOrDraining(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	if anotherProxyWrites(root) {
		t.Fatal("a proxy alone finds another writing")
	}
	for _, path := range []string{handover.ProxyLock, handover.ProxyDraining} {
		lock, err := handover.Mark(context.Background(), root.System(path))
		if err != nil {
			t.Fatal(err)
		}
		if !anotherProxyWrites(root) {
			t.Errorf("a proxy holding %s is not found writing", path)
		}
		_ = lock.Release()
	}
}
