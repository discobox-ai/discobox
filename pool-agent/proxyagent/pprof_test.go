package proxyagent

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/layout"
)

// Each unit is told its own profiling address on a pool with profiling on,
// and nothing on one with it off, so neither serves profiles unasked.
func TestUnitEnvironmentNamesProfilingAddressesOnlyWhenOn(t *testing.T) {
	off := unitEnvironment("/host", "", testProjectID, testPoolID, "", false)
	if strings.Contains(off, "PPROF") {
		t.Fatalf("a pool with profiling off told a unit to profile:\n%s", off)
	}
	on := unitEnvironment("/host", "", testProjectID, testPoolID, "", true)
	for _, want := range []string{
		EnvProxyPprofListen + "=" + PprofProxyAddress + "\n",
		EnvMediatorPprofListen + "=" + PprofMediatorAddress + "\n",
	} {
		if !strings.Contains(on, want) {
			t.Fatalf("unit environment is missing %q:\n%s", want, on)
		}
	}
}

// What a unit reads from its environment is held to loopback as the server's
// setting is; unset and off serve nothing.
func TestUnitPprofConfig(t *testing.T) {
	root := layout.Container()
	for _, value := range []string{"", "off", "OFF"} {
		t.Setenv(EnvProxyPprofListen, value)
		cfg, err := UnitPprofConfig(root, testProjectID, testPoolID, PprofProxy, EnvProxyPprofListen)
		if err != nil || cfg.Addr != "" {
			t.Fatalf("%s=%q gave %+v, %v; want nothing served", EnvProxyPprofListen, value, cfg, err)
		}
	}
	t.Setenv(EnvProxyPprofListen, PprofProxyAddress)
	cfg, err := UnitPprofConfig(root, testProjectID, testPoolID, PprofProxy, EnvProxyPprofListen)
	if err != nil || cfg.Addr != PprofProxyAddress || cfg.Setting != EnvProxyPprofListen {
		t.Fatalf("UnitPprofConfig = %+v, %v; want the proxy's address", cfg, err)
	}
	if want := root.PoolPprofToken(testProjectID, testPoolID, PprofProxy); cfg.TokenFile != want {
		t.Fatalf("token file = %q, want %q", cfg.TokenFile, want)
	}
	t.Setenv(EnvProxyPprofListen, "0.0.0.0:17092")
	if _, err := UnitPprofConfig(root, testProjectID, testPoolID, PprofProxy, EnvProxyPprofListen); err == nil || !strings.Contains(err.Error(), EnvProxyPprofListen) {
		t.Fatalf("a wildcard address gave %v, want it refused naming %s", err, EnvProxyPprofListen)
	}
}

// Each process has its own address, on loopback, apart from every other port
// the pool's processes bind.
func TestPprofAddresses(t *testing.T) {
	seen := map[string]string{
		ControlListenAddress: "proxy control API",
		ListenAddress:        "proxy",
	}
	for _, process := range []string{PprofAgent, PprofProxy, PprofMediator} {
		address, ok := PprofAddress(process)
		if !ok || !strings.HasPrefix(address, "127.0.0.1:") {
			t.Fatalf("PprofAddress(%q) = %q, %t; want a loopback address", process, address, ok)
		}
		if other, taken := seen[address]; taken {
			t.Fatalf("%s's profiling address %s is also %s's", process, address, other)
		}
		seen[address] = process
	}
	if _, ok := PprofAddress("buildkitd"); ok {
		t.Fatal("PprofAddress named a process the pool does not profile")
	}
}
