package cli

import (
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A harness is offered when a pool hosts a platform its image is published
// for, and also when nothing can rule it out: a pool that has not declared
// what it hosts yet, or a harness whose platforms nobody has read (ADR 0145 §1).
func TestPoolPlacementsRunOnlyWhatAPoolHosts(t *testing.T) {
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
	linux := placementsOf([]apimodel.Pool{pool("linux/arm64")})
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
	if !placementsOf([]apimodel.Pool{pool("linux/arm64"), pool("")}).run(harness("darwin/arm64")) {
		t.Error("an undeclared pool ruled a harness out")
	}
	if placementsOf(nil).run(harness("linux/arm64")) || placementsOf(nil).run(harness()) {
		t.Error("a harness is offered with no pool to run it")
	}
}

// A harness is offered only where one pool both hosts a platform its image is
// published for and runs the kind of image it is (ADR 26-10-09-106 §4): a
// disco-vm harness for boxd is not offered with only a Docker pool of its
// platform, and is offered beside a boxd pool. A harness from a server that
// sends no kind is OCI, one sent with an empty kind has none and runs nowhere,
// and a pool that has not said what it runs rules nothing out.
func TestPoolPlacementsRunOnlyWhatAPoolRuns(t *testing.T) {
	pool := func(platform, kind string) apimodel.Pool {
		p := apimodel.Pool{Platform: apiclientgen.NewOptString(platform)}
		if kind != "" {
			p.ImageKind = apiclientgen.NewOptString(kind)
		}
		return p
	}
	harness := func(kind string) apimodel.HarnessConfig {
		h := apimodel.HarnessConfig{Platforms: []string{"linux/amd64"}}
		if kind != "" {
			h.ImageKind = apiclientgen.NewOptString(kind)
		}
		return h
	}
	docker := placementsOf([]apimodel.Pool{pool("linux/amd64", "oci")})
	if docker.run(harness("discovm/boxd")) {
		t.Error("a disco-vm harness is offered with only a Docker pool")
	}
	if !docker.run(harness("oci")) || !docker.run(harness("")) {
		t.Error("an OCI harness is not offered on a Docker pool")
	}
	both := placementsOf([]apimodel.Pool{pool("linux/amd64", "oci"), pool("linux/amd64", "discovm/boxd")})
	if !both.run(harness("discovm/boxd")) {
		t.Error("a disco-vm harness is not offered beside a pool of its driver")
	}
	if placementsOf([]apimodel.Pool{pool("linux/amd64", "discovm/vz")}).run(harness("discovm/boxd")) {
		t.Error("a disco-vm harness is offered on a pool of another driver")
	}
	// The kind and the platform must hold of the same pool.
	split := placementsOf([]apimodel.Pool{pool("linux/arm64", "discovm/boxd"), pool("linux/amd64", "oci")})
	if split.run(harness("discovm/boxd")) {
		t.Error("a harness is offered on the strength of two pools that each fail it")
	}
	if !placementsOf([]apimodel.Pool{pool("linux/amd64", "")}).run(harness("discovm/boxd")) {
		t.Error("a pool that has not said what it runs ruled a harness out")
	}
	undeclared := apimodel.HarnessConfig{Platforms: []string{"linux/amd64"}, ImageKind: apiclientgen.NewOptString("")}
	if docker.run(undeclared) || placementsOf([]apimodel.Pool{pool("linux/amd64", "")}).run(undeclared) {
		t.Error("a harness of no declared kind is offered")
	}
}
