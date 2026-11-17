// Command discobox-main-build names and prunes main builds: the releases of
// green main commits, published apart from the tagged releases in their own
// repository (ADR 26-11-16-249).
//
//	discobox-main-build version [-repo DIR] [-rev REV]
//	discobox-main-build newer VERSION LATEST
//	discobox-main-build prune -repo OWNER/NAME -registry-owner OWNER [-keep 336h]
//
// version prints the version a main build of REV takes:
// v<next patch>-main.<first-parent count>.g<sha7>, the next patch being of the
// highest vMAJOR.MINOR.PATCH tag. It needs the whole history and every tag.
//
// newer succeeds when VERSION sorts above LATEST, the main builds'
// repository's latest release, and fails otherwise: an older build, published
// by re-running an old CI run, must not become the main channel's latest.
//
// prune deletes main builds older than -keep, always keeping the newest: each
// one's release and tag in the main builds' repository (GH_TOKEN, contents
// write there), then every ghcr image version whose tags are all main builds
// that no longer have a release (PACKAGES_TOKEN, packages write on the
// owner's discobox-* packages).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/discobox-ai/discobox/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-main-build:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: discobox-main-build version|prune [flags]")
	}
	switch args[0] {
	case "version":
		flags := flag.NewFlagSet("version", flag.ContinueOnError)
		repo := flags.String("repo", ".", "git checkout holding the commit and every v* tag")
		rev := flags.String("rev", "HEAD", "the main commit being built")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		v, err := mainVersion(ctx, *repo, *rev)
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	case "newer":
		if len(args) != 3 {
			return errors.New("usage: discobox-main-build newer VERSION LATEST")
		}
		if !newer(args[1], args[2]) {
			return fmt.Errorf("%s is not newer than %s", args[1], args[2])
		}
		return nil
	case "prune":
		flags := flag.NewFlagSet("prune", flag.ContinueOnError)
		repo := flags.String("repo", "", "owner/name of the repository holding the main builds")
		owner := flags.String("registry-owner", "", "the ghcr.io owner the main builds' images are pushed under")
		keep := flags.Duration("keep", 14*24*time.Hour, "how long a main build is kept")
		api := flags.String("api", "https://api.github.com", "the GitHub API")
		dryRun := flags.Bool("dry-run", false, "say what would be deleted, and delete nothing")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *repo == "" || *owner == "" {
			return errors.New("prune: -repo and -registry-owner are required")
		}
		p := pruner{
			releases: newClient(*api, os.Getenv("GH_TOKEN")),
			packages: newClient(*api, os.Getenv("PACKAGES_TOKEN")),
			repo:     *repo,
			owner:    *owner,
			cutoff:   time.Now().Add(-*keep),
			dryRun:   *dryRun,
			log:      os.Stdout,
		}
		return p.prune(ctx)
	default:
		return fmt.Errorf("unknown command %q: version, newer or prune", args[0])
	}
}

// mainVersion is the version of a main build of rev in the checkout at dir.
func mainVersion(ctx context.Context, dir, rev string) (string, error) {
	git := func(args ...string) (string, error) {
		//nolint:gosec // G204: git, with this tool's own arguments.
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
			}
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	sha, err := git("rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	if shallow, err := git("rev-parse", "--is-shallow-repository"); err != nil {
		return "", err
	} else if shallow == "true" {
		return "", errors.New("the checkout is shallow, so the commit count would be wrong; fetch the whole history")
	}
	count, err := git("rev-list", "--count", "--first-parent", sha)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return "", fmt.Errorf("commit count %q: %w", count, err)
	}
	tags, err := git("tag", "--list", "v*")
	if err != nil {
		return "", err
	}
	latest := highestDotRelease(strings.Fields(tags))
	if latest == "" {
		return "", errors.New("no vMAJOR.MINOR.PATCH tag to follow; fetch the tags")
	}
	return version.MainBuildVersion(latest, n, sha)
}

// newer reports whether v is a main build that sorts above latest, which is
// one when latest is no version at all (the repository has no release yet).
func newer(v, latest string) bool {
	if !version.MainBuild(v) || !semver.IsValid(v) {
		return false
	}
	return !semver.IsValid(latest) || semver.Compare(v, latest) > 0
}

// highestDotRelease is the highest exactly-vMAJOR.MINOR.PATCH tag.
func highestDotRelease(tags []string) string {
	var dots []string
	for _, tag := range tags {
		if semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == "" {
			dots = append(dots, tag)
		}
	}
	if len(dots) == 0 {
		return ""
	}
	semver.Sort(dots)
	return dots[len(dots)-1]
}
