//go:build linux && amd64

package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostCheckRejectsBrokenPasst(t *testing.T) {
	// Exercise the actual launcher and KVM handshake; software failures are
	// reported together, so no native runtime is needed for these cases.
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("host has no usable KVM: %v", err)
	}
	_ = f.Close()
	for _, tc := range []struct{ name, executable string }{
		{"wrong-format", "this is not an executable format\n"},
		{"hang", "#!/bin/sh\nexec /bin/sleep 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "passt"), []byte(tc.executable), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			start := time.Now()
			err := CheckHost(t.Context(), Config{}, true)
			if err == nil || !strings.Contains(err.Error(), "run passt --version") {
				t.Fatalf("error = %v, want passt execution failure", err)
			}
			if elapsed := time.Since(start); elapsed > 8*time.Second {
				t.Fatalf("host check exceeded its bound: %s", elapsed)
			}
		})
	}
}
