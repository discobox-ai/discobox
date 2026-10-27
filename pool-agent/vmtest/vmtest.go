// Package vmtest is test support for code that drives disco-vm machines: it
// lets a test binary be the guest agent of disco-vm's fake driver, as
// `disco-vm guest` is, and the shim that holds a fake machine, so a test
// boots fake machines through the engine and reaches them with the same guest
// operations a real machine answers. Only tests import it.
package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// shimSubcommand is the test binary run as a fake machine's shim.
const shimSubcommand = "__vmtest-shim"

// RunFakeGuestIfInvoked runs this process as a fake machine's guest agent, or
// as the shim that holds one, and exits, when the fake driver started it as
// one, and returns immediately otherwise. A test binary's TestMain calls it
// first.
func RunFakeGuestIfInvoked() {
	if len(os.Args) < 2 {
		return
	}
	var err error
	switch os.Args[1] {
	case "guest":
		err = runFakeGuest(os.Args[2:])
	case shimSubcommand:
		err = runShim(os.Args[2:])
	default:
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake "+os.Args[1]+":", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runFakeGuest is `disco-vm guest` with the flags the fake driver passes.
func runFakeGuest(args []string) error {
	flags := flag.NewFlagSet("guest", flag.ContinueOnError)
	listen := flags.String("listen", "", "")
	addrFile := flags.String("addr-file", "", "")
	root := flags.String("root", "", "")
	isFake := flags.Bool("fake", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	listener, err := guest.Listen(*listen)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*root, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*addrFile+".tmp", []byte(listener.Addr().String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(*addrFile+".tmp", *addrFile); err != nil {
		return err
	}
	// A fake guest's shutdown exits the process; Serve never returns from it.
	return (&guest.Server{Version: "test", Root: *root, Fake: *isFake}).Serve(listener)
}

// runShim makes the fake driver again on the directory and project it was
// given, as its owner, and holds the instance it was started for.
func runShim(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: <dir> <project> <shim arguments>")
	}
	driver, err := newFakeDriver(args[0], args[1], nil)
	if err != nil {
		return err
	}
	return local.ShimMain(context.Background(), driver, args[2:])
}

// FakeDriver is disco-vm's fake driver for project, keeping its machines in
// dir, calling with keys (nil is as the directory's owner), and offering one
// base of this host's OS, named and IDed base: a test boots a machine from it
// as a sandbox's or a pool's machine boots a driver's base. Its machines' shims
// are this test binary, whose TestMain calls RunFakeGuestIfInvoked.
func FakeDriver(t testing.TB, dir, project string, keys machine.KeySource) machine.Driver {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("disco-vm's fake guests run POSIX processes here")
	}
	bases, err := json.Marshal([]machine.Base{{Name: Base, ID: Base, GuestOS: machine.OS(runtime.GOOS)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fake.BasesEnv, string(bases))
	driver, err := newFakeDriver(dir, project, keys)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

// Base is the name and ID of FakeDriver's one base.
const Base = "base"

func newFakeDriver(dir, project string, keys machine.KeySource) (machine.Driver, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return machine.New("fake", machine.Config{
		Project:     project,
		Dir:         dir,
		ShimCommand: []string{exe, shimSubcommand, dir, project},
		Keys:        keys,
	})
}
