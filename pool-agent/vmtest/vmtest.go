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
	_ "github.com/discobox-ai/vm/pkg/machine/fake" // registers the driver FakeDriver makes by name
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

// runShim makes the fake driver again on the directory, pool and
// configuration it was given, as its owner, and holds the instance it was
// started for. The configuration must be the one the instance was started
// with, which the local driver checks.
func runShim(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: <dir> <pool> <driver configuration> <shim arguments>")
	}
	var config json.RawMessage
	if args[2] != "" {
		config = json.RawMessage(args[2])
	}
	driver, err := newFakeDriver(args[0], args[1], nil, config)
	if err != nil {
		return err
	}
	return local.ShimMain(context.Background(), driver, args[3:])
}

// FakeDriver is disco-vm's fake driver for pool, keeping its machines in
// dir and calling with keys (nil is as the directory's owner), whose base is
// Base: a test makes a fresh machine from it as a sandbox's or a pool's
// machine is made from a driver's base. config is the fake's own
// configuration (machine.Config.DriverConfig; nil is its defaults, which
// commit), such as {"noCommit":true}: the driver reads no environment for
// it. Its machines' shims are this test binary, whose TestMain calls
// RunFakeGuestIfInvoked, given the same configuration.
func FakeDriver(t testing.TB, dir, pool string, keys machine.KeySource, config json.RawMessage) machine.Driver {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("disco-vm's fake guests run POSIX processes here")
	}
	driver, err := newFakeDriver(dir, pool, keys, config)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

// Base is FakeDriver's base.
const Base = "base"

func newFakeDriver(dir, pool string, keys machine.KeySource, config json.RawMessage) (machine.Driver, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// The fake's configuration holds no secret, so its shim is given it on
	// its command line.
	return machine.New("fake", machine.Config{
		Pool:         pool,
		Dir:          dir,
		Base:         Base,
		ShimCommand:  []string{exe, shimSubcommand, dir, pool, string(config)},
		Keys:         keys,
		DriverConfig: config,
	})
}
