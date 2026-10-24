package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/poolassets"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/sandbox-agent/bundlestore"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// runProvisionCommand is the discobox-provision personality: how anything in
// the box waits for a feature still being provisioned (ADR 26-10-16-423 §5),
// what a placeholder command runs, and the one command whoever makes a box
// runs in it (ADR 26-10-24-953 §§2, 4).
//
//	discobox-provision wait FEATURE...           block until each is ready
//	discobox-provision run FEATURE COMMAND ARG... a placeholder: wait, then exec
//	discobox-provision identity --key PATH       the box's public key, made on demand
//	discobox-provision apply [POOL FLAGS]         provision the box its maker placed
func runProvisionCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: discobox-provision wait FEATURE...\n       discobox-provision run FEATURE COMMAND [ARG...]\n       discobox-provision identity --key PATH\n       discobox-provision apply [--control-plane URL --project ID --pool ID --key PATH]")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "wait":
		flags := flag.NewFlagSet("discobox-provision wait", flag.ContinueOnError)
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() == 0 {
			return usage()
		}
		return provisionWait(ctx, flags.Args())
	case "run":
		if len(args) < 3 {
			return usage()
		}
		return provisionRun(ctx, args[1], args[2], args[3:])
	case "identity":
		flags := flag.NewFlagSet("discobox-provision identity", flag.ContinueOnError)
		key := flags.String("key", "", "the box's identity key file")
		if err := flags.Parse(args[1:]); err != nil || *key == "" || flags.NArg() != 0 {
			return usage()
		}
		return provisionIdentity(*key, os.Stdout)
	case "apply":
		flags := flag.NewFlagSet("discobox-provision apply", flag.ContinueOnError)
		var pool poolApply
		flags.StringVar(&pool.controlPlane, "control-plane", "", "a pool's control plane URL, whose asset routes it fetches its bundles from")
		flags.StringVar(&pool.project, "project", "", "the pool's project")
		flags.StringVar(&pool.pool, "pool", "", "the pool")
		flags.StringVar(&pool.key, "key", "", "the pool's identity key file, which discobox-provision identity made")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usage()
		}
		if pool == (poolApply{}) {
			return applySandbox(ctx)
		}
		if pool.controlPlane == "" || pool.project == "" || pool.pool == "" || pool.key == "" {
			return usage()
		}
		return applyPool(ctx, pool)
	}
	return usage()
}

// provisionIdentity prints the public half of the box's identity key at
// path, making the key there first when the box has none (ADR 26-10-24-953
// §4). The private half never leaves the box, and asking again answers the
// same key.
func provisionIdentity(path string, out io.Writer) int {
	key, _, err := poolauth.LoadOrCreateKey(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	public, err := poolauth.PublicKeyOf(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	fmt.Fprintln(out, public)
	return 0
}

// poolApply is what a pool's maker tells apply: where the pool's bundles are
// fetched from, and as which pool, signing with the key identity made.
type poolApply struct {
	controlPlane string
	project      string
	pool         string
	key          string
}

// applyPool provisions a pool's box from the pins its maker placed (ADR
// 26-10-24-953 §2): it fetches each bundle from the server's asset routes as
// the pool — enrolled before this runs, so its key is the pool's already —
// and applies every feature, every step blocking, then exits. The pool agent
// is what the pool's bundles lay and start; this never runs as the agent.
func applyPool(ctx context.Context, pool poolApply) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	key, err := poolauth.LoadKey(pool.key)
	if err != nil {
		return fail(fmt.Errorf("the pool's identity key, which its maker enrolls before it applies: %w", err))
	}
	base, client, err := wire.HTTPClient(pool.controlPlane, 0)
	if err != nil {
		return fail(fmt.Errorf("control plane URL: %w", err))
	}
	pins, ok, err := placedPins()
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("the box has no pins (%s) to apply", agentlayer.PinsFile))
	}
	fetcher := &poolassets.Client{BaseURL: base, HTTP: client, ProjectID: pool.project, PoolID: pool.pool, Key: key}
	if err := applyBundles(ctx, pins, fetcher); err != nil {
		return fail(err)
	}
	trees, err := bundlestore.Trees("/")
	if err != nil {
		return fail(err)
	}
	if err := provision.Pool(ctx, slog.Default(), provision.NewTracker(provisionspec.StatusPath), trees); err != nil {
		return fail(err)
	}
	return 0
}

// applySandbox starts a sandbox's agent on a box that is already running,
// whose maker placed the agent, its units and their enablement, the pins and
// the bootstrap (ADR 26-10-24-953 §2): systemd is told of the units, and the
// agent's path unit started, which starts the agent once its bootstrap is
// there. The agent then provisions the box itself, from its first
// runtime-config document. A box systemd is not running has nothing to start:
// it boots on the units it was placed.
func applySandbox(ctx context.Context) int {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return 0
	}
	for _, command := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "start", "discobox-sandbox-agent.path"},
	} {
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		//nolint:gosec // G204: the commands are the fixed list above.
		cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "discobox-provision: %s: %v\n", strings.Join(command, " "), err)
			return 1
		}
	}
	return 0
}

func provisionWait(ctx context.Context, features []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, features, func(status provisionspec.Status, pending []string) {
		fmt.Fprintf(os.Stderr, "discobox-provision: waiting for %s\n", provision.Describe(status, pending))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	code := 0
	for _, name := range features {
		if err := provision.Problem(status, name); err != nil {
			fmt.Fprintln(os.Stderr, "discobox-provision:", err)
			code = 1
		}
	}
	return code
}

// provisionRun is a placeholder's: say the feature is still provisioning,
// wait for it, and run the real command in this process's place.
func provisionRun(ctx context.Context, feature, command string, args []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, []string{feature}, func(status provisionspec.Status, pending []string) {
		fmt.Fprintf(os.Stderr, "discobox: %s is not installed yet: the %s feature is still provisioning. Waiting for it: %s\n", command, feature, provision.Describe(status, pending))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox:", err)
		return 127
	}
	if err := provision.Problem(status, feature); err != nil {
		fmt.Fprintf(os.Stderr, "discobox: %s is not installed: %v\n", command, err)
		return 127
	}
	path, err := provision.LookPath(command, os.Getenv("PATH"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox: the %s feature is ready, but %v\n", feature, err)
		return 127
	}
	argv := append([]string{command}, args...)
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox: run %s: %v\n", path, err)
		return 126
	}
	return 0
}
