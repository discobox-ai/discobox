package discovm

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// fakeImage is the fake driver's pool image spec in a test checkout.
const fakeImage = "server/providers/discovm/images/fake/pool.yaml"

// The fake driver is disco-vm's machine with no hypervisor: a guest is a
// disco-vm agent process on this host, confined to a directory. It hosts a pool
// the way boxd does, in a machine of its own, so the engine, its shim, and the
// guest protocol all run here as they do against a real machine.
//
// It is in the test binary only. Its guest agent is this binary (fake.New's
// default), so a server built with it would start its guests as servers.
func init() {
	drivers["fake"] = driverDefinition{
		machine: func() (machine.Driver, error) { return fake.New() },
		pools: func(e *engine.Engine) driver {
			return &poolMachine{
				engine: e,
				shell:  []string{"/bin/sh"},
				logs: func(opts sandbox.PoolLogOptions) []string {
					return []string{"sh", "-c", fmt.Sprintf("seq 1 5 | tail -n %d", opts.Tail)}
				},
				logSource: "fake machine log",
				built:     []guestImage{{Tag: poolImage, Spec: fakeImage}},
			}
		},
	}
}

// TestMain lets this binary be the two processes the engine starts: a shim, as
// the server is, and a fake guest's agent, as `disco-vm guest` is.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "guest" {
		if err := runFakeGuest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fake guest:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	RunShimIfInvoked()
	os.Exit(m.Run())
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
