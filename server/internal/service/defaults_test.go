package service

import (
	"testing"
)

// The libkrun instance a first start installs carries no configuration: the
// image it boots brings its own runtime, so nothing is installed or named on
// the host (ADR 26-09-23-473 §5).
func TestDefaultLibkrunInstanceNeedsNoConfiguration(t *testing.T) {
	provider := defaultSandboxProvider("proj_1", "prov_1", "libkrun")
	if provider.Type != "libkrun" || provider.Disabled {
		t.Fatalf("provider = %+v, want an enabled libkrun instance", provider)
	}
	if len(provider.Config) != 0 {
		t.Fatalf("config = %s, want none", provider.Config)
	}
}
