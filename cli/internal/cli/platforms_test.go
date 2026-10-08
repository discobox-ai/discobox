package cli

import (
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A harness is offered when a pool hosts a platform its image is published
// for, and also when nothing can rule it out: a pool that has not declared
// what it hosts yet, or a harness whose platforms nobody has read (ADR 0145 §1).
func TestPoolPlatformsRunOnlyWhatAPoolHosts(t *testing.T) {
	pool := func(platform string) apimodel.Pool {
		p := apimodel.Pool{}
		if platform != "" {
			p.Platform = apiclientgen.NewOptString(platform)
		}
		return p
	}
	harness := func(platforms ...string) apimodel.HarnessConfig {
		return apimodel.HarnessConfig{Platforms: platforms}
	}
	linux := platformsOf([]apimodel.Pool{pool("linux/arm64")})
	if !linux.run(harness("linux/arm64")) {
		t.Error("a harness of the pool's platform is not offered")
	}
	if linux.run(harness("darwin/arm64")) || linux.run(harness("linux/amd64")) {
		t.Error("a harness of another platform is offered")
	}
	if !linux.run(harness()) {
		t.Error("a harness whose platforms nobody has read is not offered")
	}
	if !linux.run(harness("linux/amd64", "linux/arm64")) {
		t.Error("a multi-arch harness is not offered on a pool of one of its platforms")
	}
	if !platformsOf([]apimodel.Pool{pool("linux/arm64"), pool("")}).run(harness("darwin/arm64")) {
		t.Error("an undeclared pool ruled a harness out")
	}
	if platformsOf(nil).run(harness("linux/arm64")) || platformsOf(nil).run(harness()) {
		t.Error("a harness is offered with no pool to run it")
	}
}
