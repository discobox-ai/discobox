// Package vmtest is test support for code that drives disco-vm machines: it
// lets a test binary be the guest agent of disco-vm's fake driver, as
// `disco-vm guest` is, so a test boots fake machines through the engine and
// reaches them with the same guest operations a real machine answers. Only
// tests import it.
package vmtest

import (
	"flag"
	"fmt"
	"os"

	"github.com/discobox-ai/vm/pkg/guest"
)

// RunFakeGuestIfInvoked runs this process as a fake machine's guest agent and
// exits, when the fake driver started it as one (its "guest" argument), and
// returns immediately otherwise. A test binary's TestMain calls it first.
func RunFakeGuestIfInvoked() {
	if len(os.Args) < 2 || os.Args[1] != "guest" {
		return
	}
	if err := runFakeGuest(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "fake guest:", err)
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
