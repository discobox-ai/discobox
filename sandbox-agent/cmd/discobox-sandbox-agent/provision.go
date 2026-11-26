package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/poolauth"
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
//	discobox-provision retry                     run what failed again, and wait for it
//	discobox-provision run FEATURE COMMAND ARG... a placeholder: wait, then exec
//	discobox-provision identity --key PATH       the box's public key, made on demand
//	discobox-provision apply [--wait] [--restart-agent]  provision the box its maker placed
//	discobox-provision can-mount-in-place        exits 0: apply wires a running machine in place
func runProvisionCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: discobox-provision wait FEATURE...\n       discobox-provision retry\n       discobox-provision run FEATURE COMMAND [ARG...]\n       discobox-provision identity --key PATH\n       discobox-provision apply [--wait] [--restart-agent]\n       discobox-provision can-mount-in-place")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "configure":
		if len(args) != 2 || args[1] != "docker" {
			return usage()
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		plan, err := provision.WaitConfigurationPlan(ctx, provision.ConfigurationPlanPath)
		if err == nil {
			platform, platformErr := provision.DetectPlatform("/")
			err = platformErr
			if err == nil {
				runner := &provision.Runner{Root: "/", Records: provision.Records{Dir: provision.RecordsDir}, Platform: platform, Booted: true, LogDir: provision.LogDir}
				err = runner.Configure(ctx, plan.Features, plan.Options, "docker")
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "discobox-provision: Docker configuration:", err)
			return 1
		}
		return 0
	case "wait":
		flags := flag.NewFlagSet("discobox-provision wait", flag.ContinueOnError)
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() == 0 {
			return usage()
		}
		return provisionWait(ctx, flags.Args())
	case "retry":
		if len(args) != 1 {
			return usage()
		}
		return provisionRetry(ctx, provision.RetryDir, provisionspec.StatusPath)
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
	case "outbound-probe":
		raw, err := os.ReadFile(pooloutbound.RuntimeFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		var outbound pooloutbound.Resolved
		if err := json.Unmarshal(raw, &outbound); err != nil {
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(pooloutbound.Check(ctx, outbound)); err != nil {
			return 1
		}
		return 0
	case "apply":
		flags := flag.NewFlagSet("discobox-provision apply", flag.ContinueOnError)
		wait := flags.Bool("wait", false, "also wait for background provisioning to finish")
		restartAgent := flags.Bool("restart-agent", false, "restart the agent where it runs, onto what was placed")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usage()
		}
		return provisionApply(ctx, *wait, *restartAgent)
	case "can-mount-in-place":
		// What a machine's pool asks the agent it placed before it applies
		// (ADR 26-11-23-458 §6): this agent's apply wires a running machine
		// in place (applyDeliveredBundles). One from before answers its
		// usage, and the pool restarts the machine onto it instead.
		if len(args) != 1 {
			return usage()
		}
		return 0
	}
	return usage()
}

// provisionApply applies a box's bundles: the verified contents its maker
// placed, or, on a box delivered without them, the agent's start. Either
// runs systemctl on a box systemd is running, and apply runs straight after
// the box's start, which a provider may return from once the machine runs
// (boxd) or once systemd is launched (archil), so systemd's bus may not
// answer yet, and on archil its directory under /run may not be there yet
// either. So wherever systemd runs, as PID 1 or by that directory, it waits
// for the bus first, rather than failing the start on the first systemctl
// that cannot reach it. Once the bus answers the directory is there, so
// every later reading of it (provision.Apply's booted, the delivered path's
// own check) agrees with this one; an image's build, where systemd is not
// PID 1, waits for nothing.
func provisionApply(ctx context.Context, wait, restartAgent bool) int {
	if _, err := os.Stat("/run/systemd/system"); err == nil || systemdIsInit() {
		busCtx, cancel := context.WithTimeout(ctx, systemdReadyTimeout)
		err = awaitSystemdBus(busCtx)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "discobox-provision:", err)
			return 1
		}
	}
	if info, err := os.Stat(agentlayer.PlacedDir); err == nil && info.IsDir() {
		return applyPlacedBundles(ctx)
	} else if err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return applyDeliveredBundles(ctx, wait, restartAgent)
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

// applyPlacedBundles applies the verified contents its maker placed (ADR
// 26-10-24-953 §2, ADR 26-10-30-875 §5): the pins, and every blob of their
// bundles for the box's platform (agentlayer.PlacedDir), which it applies to
// the box's content store with nothing fetched — it dials nothing, since it
// runs before the pool agent and so before any channel to the control plane
// — then applies every feature, every step blocking, and exits. The pool
// agent is what the pool's bundles lay and start; this never runs as the
// agent.
func applyPlacedBundles(ctx context.Context) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	// Managed outbound material is installed before bundle package steps fetch.
	revision := os.Getenv(pooloutbound.RevisionEnv)
	if len(revision) != 64 || strings.Trim(revision, "0123456789abcdef") != "" {
		return fail(fmt.Errorf("missing managed pool outbound revision; the pool must be provisioned by an updated server"))
	}
	if raw, err := os.ReadFile(pooloutbound.StagedRuntimeFile(revision)); err == nil {
		var outbound pooloutbound.Resolved
		if err := json.Unmarshal(raw, &outbound); err != nil {
			return fail(fmt.Errorf("invalid managed pool outbound material"))
		}
		if err := outbound.ValidateEnvironment(os.LookupEnv); err != nil {
			return fail(err)
		}
		//nolint:gosec // Fixed private script placed by the authenticated pool maker.
		command := exec.CommandContext(ctx, "sh", pooloutbound.InstallFile)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return fail(fmt.Errorf("install pool outbound trust: %w", err))
		}
	} else {
		return fail(err)
	}
	pins, ok, err := agentlayer.ReadPins(agentlayer.PinsFile)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("the box has no pins (%s) to apply", agentlayer.PinsFile))
	}
	store := &bundlestore.Store{Root: "/", Fetcher: bundlestore.PlacedFetcher{Root: "/"}, Platform: imagecache.PoolPlatform(), Logger: slog.Default()}
	err = store.Apply(ctx, pins)
	// Keep the local-delivery directory across retries after consuming blobs.
	if err == nil {
		entries, readErr := os.ReadDir(agentlayer.PlacedDir)
		if readErr != nil {
			return fail(readErr)
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(agentlayer.PlacedDir, entry.Name())); err != nil {
				return fail(err)
			}
		}
	}
	if err != nil {
		return fail(fmt.Errorf("apply the box's placed bundles: %w", err))
	}
	trees, err := bundlestore.Trees("/")
	if err != nil {
		return fail(err)
	}
	if err := provision.Apply(ctx, slog.Default(), provision.NewTracker(provisionspec.StatusPath), trees, nil, provision.Options{}, false); err != nil {
		return fail(err)
	}
	return 0
}

// applyDeliveredBundles starts a sandbox's agent on a box that is already running,
// whose maker placed the agent, its units and their enablement, the pins and
// the bootstrap (ADR 26-10-24-953 §2): systemd is told of the units, and the
// agent's path unit started, which starts the agent once its bootstrap is
// there. The agent then provisions the box itself, from its first
// runtime-config document. A box systemd is not running has nothing to start:
// it boots on the units it was placed.
//
// A machine sandbox's pool places the wiring in place
// (agentlayer.WireInPlaceUnit), which this starts and waits for before the
// agent: the machine is provisioned while it runs, never restarted for its
// boot to mount (ADR 26-11-23-458 §2). It mounts nothing where the mounts
// are unchanged. restartAgent is a placement that moved: an agent running on
// the one before is stopped before the mounts and started after, on what
// was placed now (§4). Its path unit goes with it, or it would start the
// agent again at once. No other service is stopped or restarted.
func applyDeliveredBundles(ctx context.Context, wait, restartAgent bool) int {
	requested, _, err := agentlayer.ReadPins(agentlayer.PinsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		if wait || !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "discobox-provision: cannot wait for provisioning before systemd is running")
			return 1
		}
		return 0
	}
	type step struct {
		argv []string
		// timeout bounds the step; zero is ctx's own, for the wiring in
		// place, which moves what a machine has at each path into its
		// volume, as long as that takes.
		timeout time.Duration
	}
	steps := []step{{argv: []string{"systemctl", "daemon-reload"}, timeout: time.Minute}}
	if restartAgent {
		steps = append(steps, step{argv: []string{"systemctl", "stop", agentlayer.AgentStartUnit, agentlayer.AgentServiceUnit}, timeout: time.Minute})
	}
	if _, err := os.Stat(wireInPlacePath); err == nil {
		steps = append(steps, step{argv: []string{"systemctl", "start", agentlayer.WireInPlaceUnit}})
	}
	steps = append(steps, step{argv: []string{"systemctl", "start", agentlayer.AgentStartUnit}, timeout: time.Minute})
	for _, s := range steps {
		runCtx, cancel := ctx, context.CancelFunc(func() {})
		if s.timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, s.timeout)
		}
		//nolint:gosec // G204: the commands are the fixed list above.
		cmd := exec.CommandContext(runCtx, s.argv[0], s.argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "discobox-provision: %s: %v\n", strings.Join(s.argv, " "), err)
			return 1
		}
	}
	if wait {
		status, err := provision.Wait(ctx, provisionspec.StatusPath, nil, false, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if status.State == provisionspec.StateFailed {
			fmt.Fprintln(os.Stderr, "discobox-provision: provisioning failed", status.Error)
			for name, feature := range status.Features {
				if feature.State == provisionspec.StateFailed {
					fmt.Fprintln(os.Stderr, provision.Problem(status, name))
				}
			}
			return 1
		}
		if len(requested) > 0 {
			current, err := (&bundlestore.Store{Root: "/"}).Current(requested)
			if err != nil || !current {
				fmt.Fprintln(os.Stderr, "discobox-provision: requested bundle pins have not been applied; restart the box to apply changed pins")
				return 1
			}
		}
	}

	return 0
}

// wireInPlacePath is agentlayer.WireInPlacePath. A variable so a test does
// not read the machine's own.
var wireInPlacePath = agentlayer.WireInPlacePath

func provisionWait(ctx context.Context, features []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, features, false, func(status provisionspec.Status, pending []string) {
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

// provisionRetry is a reprovision asked from inside the box (ADR
// 26-11-11-560 §5): the agent runs every failed feature again, and this
// waits for the run as `wait` does. It fails naming each feature still
// failed once the run is done.
func provisionRetry(ctx context.Context, dir, statusPath string) int {
	if err := provision.RequestRetry(ctx, dir); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	status, err := provision.Wait(ctx, statusPath, nil, false, func(status provisionspec.Status, _ []string) {
		var pending []string
		for name, feature := range status.Features {
			if !feature.State.Done() {
				pending = append(pending, name)
			}
		}
		fmt.Fprintf(os.Stderr, "discobox-provision: provisioning %s\n", provision.Describe(status, pending))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	code := 0
	if !status.Ready && status.Error != "" {
		fmt.Fprintln(os.Stderr, "discobox-provision: provisioning failed:", status.Error)
		code = 1
	}
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		if err := provision.Problem(status, name); err != nil && status.Features[name].State == provisionspec.StateFailed {
			fmt.Fprintln(os.Stderr, "discobox-provision:", err)
			code = 1
		}
	}
	if code == 0 {
		fmt.Fprintln(os.Stderr, "discobox-provision: every feature is provisioned")
	}
	return code
}

// provisionRun is a placeholder's: say the feature is still provisioning,
// wait for it, and run the real command in this process's place.
func provisionRun(ctx context.Context, feature, command string, args []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, []string{feature}, true, func(status provisionspec.Status, pending []string) {
		if status.Retry != nil && status.Features[feature].State == provisionspec.StateFailed {
			fmt.Fprintf(os.Stderr, "discobox: the %s feature failed to provision. Waiting for the automatic retry\n", feature)
			return
		}
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
