package sandboxruntime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/docker"

	"github.com/discobox-ai/discobox/pkg/layout"
)

// dockerImageEnv names a registry image, such as debian:sid, that the
// docker driver's machines are made fresh from in TestMachineNewRootOnDocker,
// against the daemon $DOCKER_HOST names: the variable disco-vm's own docker
// conformance suite reads. The driver also needs the disco-vm agent built for
// linux ($DISCO_VM_AGENT), and a rootful daemon on a cgroup v2 host. The
// driver reads none of these itself: the test builds its configuration from
// them, as disco-vm's command line does.
const dockerImageEnv = "DISCO_VM_DOCKER_IMAGE"

// On disco-vm's docker driver, a sandbox's machine is made with a data and a
// cache volume, and a new root keeps both and loses what was on the old one
// (disco-vm ADR 26-11-03-782). The pool's sandbox agent is not run: a docker
// guest's service is reached through a port the daemon publishes, where the
// fake agent these tests serve on the host cannot listen, so this drives the
// runtime's create and upgrade and reads the machine through its guest.
func TestMachineNewRootOnDocker(t *testing.T) {
	image := os.Getenv(dockerImageEnv)
	if image == "" {
		t.Skipf("set %s (an image, such as debian:sid) and %s to run a sandbox's machine on disco-vm's docker driver", dockerImageEnv, docker.AgentEnv)
	}
	values := machine.ConfigFromEnv("docker")
	if values["root"] == "" {
		// Not t.TempDir: the driver's helper container writes it as the
		// daemon's root, and only the driver removes what it made there.
		root, err := os.MkdirTemp("", "sbxrt-docker-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		values["root"] = root
	}
	config, err := machine.ConfigFromValues("docker", values)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool := "sbxrt-new-root"
	driver, err := docker.New(machine.Config{Pool: pool, Base: image, DriverConfig: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Check(ctx); err != nil {
		t.Skipf("the docker driver cannot run here: %v", err)
	}
	e, err := engine.Open(driver, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		instances, _ := driver.Instances(context.Background())
		for _, inst := range instances {
			_ = e.Remove(context.Background(), inst, true)
		}
		_ = driver.DeleteCache(context.Background(), "sbx-docker")
	})
	r, err := NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Audit: &fakeAuditPurger{}, Engine: e, Base: image, Root: layout.ContainerAt(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}

	inst, _, err := r.ensureInstance(ctx, "sbx_docker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Data == nil || inst.Cache == nil || inst.Cache.Key != "sbx-docker" {
		t.Fatalf("machine volumes = %+v, %+v; want a data volume and a cache of its own", inst.Data, inst.Cache)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	// What the sandbox agent records once it has wired the volume.
	guest, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.CopyTo(ctx, "/run/discobox", fileTree(t, "machine-wired", "start-1\n")); err != nil {
		t.Fatal(err)
	}
	if err := guest.CopyTo(ctx, "/.discobox/data", fileTree(t, ".discobox-wired", "sbx_docker start-1 working-root\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.writeMachineRecord("sbx_docker", machineRecord{Instance: inst.ID}); err != nil {
		t.Fatal(err)
	}
	if err := r.recordDataOnVolume(ctx, "sbx_docker", inst); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx, inst, time.Minute); err != nil {
		t.Fatal(err)
	}

	record, _, err := r.readMachineRecord("sbx_docker")
	if err != nil || !record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want its data on its volume read from it", record, err)
	}
	r.remakeOnNewRoot(ctx, inst, &record)
	if !record.NewRoot {
		t.Fatal("a machine with a data volume was not asked a new root")
	}
	if err := r.writeMachineRecord("sbx_docker", record); err != nil {
		t.Fatal(err)
	}
	upgraded, err := r.upgradeMachine(ctx, "sbx_docker", inst, &record)
	if err != nil {
		t.Fatal(err)
	}
	if saved, _, err := r.readMachineRecord("sbx_docker"); err != nil || saved.NewRoot || saved.Instance != upgraded.ID {
		t.Fatalf("machine record = %+v, %v; want the new root's instance and no upgrade left", saved, err)
	}
	if err := e.Start(ctx, upgraded, engine.StartOptions{Timeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if guest, err = e.Guest(ctx, upgraded); err != nil {
		t.Fatal(err)
	}
	if got := readMachineFile(t, guest, "/.discobox/data/kept.txt"); got != "the sandbox's data" {
		t.Fatalf("data volume file = %q, want it kept", got)
	}
	if got := readMachineFile(t, guest, "/.discobox/cache/kept.txt"); got != "a warm cache" {
		t.Fatalf("cache volume file = %q, want it kept", got)
	}
	if machineHasFile(t, r, upgraded, "/opt/kept.txt") {
		t.Fatal("the old root's file is in the machine, want a new root")
	}
	// The new root's start has a new /run, so with no agent to wire it the
	// volume's line names a start that is over.
	if err := r.recordDataOnVolume(ctx, "sbx_docker", upgraded); err != nil {
		t.Fatal(err)
	}
	if saved, _, err := r.readMachineRecord("sbx_docker"); err != nil || saved.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want no data on its volume recorded for a start nothing wired", saved, err)
	}
}
