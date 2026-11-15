// Command discobox-vm-pins refuses a release whose pinned pool VM guest or
// libkrun kernel is older than vm-image/ at the commit being released.
//
// The server pins both by digest, and each is published from its own git tag
// line (vm/vN, vm-kernel/vN), so nothing else notices when vm-image/ moves on
// and the pin does not. For each pin this finds the tag whose published image
// has the pinned digest, takes that tag's commit as the one the image was built
// from, and fails when the pin's paths differ between it and the release
// commit.
//
// A deliberate exception is a Release-Stale-VM-Pin: <reason> trailer in the
// release tag's annotation — never a commit message, which whoever wrote a
// merged pull request controls, while only the owner cuts a release tag. It
// turns the refusal into a warning and a note, written to -notes, that the
// release carries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// overrideTrailer is the trailer that releases despite a stale pin.
const overrideTrailer = "Release-Stale-VM-Pin"

// pin is one digest the server pins and the vm-image/ paths it is built from.
type pin struct {
	// Const is the Go constant holding the pinned reference, declared in File.
	Const string
	File  string
	// TagPrefix scopes the pin's release line in the git tag namespace; the
	// image tag is the git tag without it (vm/v7 publishes :v7).
	TagPrefix string
	// Paths is the git pathspec whose changes the pinned image must include:
	// its build's inputs, as its workflow's paths filter lists them, less the
	// documentation and tests that never reach the image.
	Paths []string
}

// notBuilt excludes what sits among a build's inputs but never reaches its
// image, so an edit to a note or a test does not hold a release.
var notBuilt = []string{":(exclude)*.md", ":(exclude)*_test.go"}

// pins are the two release lines vm:publish and vm:publish-kernel push. Each
// one's Paths are its workflow's paths filter (vm-image.yml, vm-kernel.yml),
// less the workflow file itself; vm-image/REVIEW.md keeps them in step.
var pins = []pin{
	{
		Const:     "DefaultVMImage",
		File:      "server/providers/guestimage/image.go",
		TagPrefix: "vm/",
		Paths: append([]string{
			"vm-image", ":(exclude)vm-image/kernel",
			// Compiled into the guest as discobox-vsock-guest.
			"pkg/vsock", "pool-agent/cmd/discobox-vsock-guest",
		}, notBuilt...),
	},
	{
		Const:     "DefaultKernelImage",
		File:      "server/providers/libkrun/provider.go",
		TagPrefix: "vm-kernel/",
		Paths:     append([]string{"vm-image/kernel"}, notBuilt...),
	},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], registry{}); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-vm-pins:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, reg resolver) error {
	flags := flag.NewFlagSet("discobox-vm-pins", flag.ContinueOnError)
	repo := flags.String("repo", ".", "git checkout holding the release commit and the vm/ and vm-kernel/ tags")
	rev := flags.String("rev", "", "the release tag (or any commit-ish) being released")
	notes := flags.String("notes", "", "file to write the release-notes record of an override to (written empty when there is none)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("-rev is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	report, err := check(ctx, git{dir: *repo}, reg, *rev)
	if err != nil {
		return err
	}
	text := report.String()
	fmt.Print(text)
	if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
		if err := appendFile(summary, report.Markdown()); err != nil {
			return err
		}
	}
	if *notes != "" {
		if err := os.WriteFile(*notes, []byte(report.Notes()), 0o600); err != nil {
			return err
		}
	}
	if !report.Releasable() {
		return fmt.Errorf("refusing to release %s: %s", *rev, strings.Join(report.StaleConsts(), " and ")+" stale (see above); "+
			"publish and pin a new image, or add a "+overrideTrailer+": <reason> trailer to the tag's annotation")
	}
	return nil
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
