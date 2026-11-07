package provision

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// A client can activate the image's enabled docker.socket as soon as exec
// readiness is released. Exercise the shipped feature, with system commands
// intercepted so this test never changes the host's Docker installation.
func TestDockerProxyInstalledBeforeSocketSetup(t *testing.T) {
	b := newBox(t)
	copyTestTree(t, "../features/docker", filepath.Join(b.tree, provisionspec.FeaturesDir, "docker"))
	tracker := b.tracked()
	b.runner.Booted = true
	observed := false
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, _ []string, name string, args ...string) error {
		for _, arg := range append([]string{name}, args...) {
			if filepath.Base(arg) != "socket-activation.sh" {
				continue
			}
			observed = true
			ready := tracker.Status().Ready
			_, err := os.Stat(filepath.Join(b.root, "etc/systemd/system/docker.service.d/proxy.conf"))
			if os.IsNotExist(err) {
				t.Errorf("Docker socket setup reached without proxy drop-in (exec readiness released: %v)", ready)
			} else if err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	b.mustApply()
	if !observed {
		t.Fatal("Docker socket setup was not exercised")
	}
}
