package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/x/gitutil"
)

// Headers the guest image route sets. They mirror the server's constants: the
// destination is known before the build starts, and the outcome can only be a
// trailer, because the response began minutes before the build ended.
const (
	guestImageDestinationHeader = "X-Discobox-Guest-Image-Destination"
	guestImageErrorTrailer      = "X-Discobox-Guest-Image-Error"
)

// newPoolGuestImageCommand implements `discobox admin pool build-guest`: rebuild
// the VM image a pool's backend boots, on that pool's own VM.
//
// This is the macOS bootstrap closing on itself (ADR 0062 §7). A Mac has no
// Docker daemon, so the only builder that can produce a guest image is the one
// inside a pool VM — a VM booted from the guest image being replaced. The
// running guest builds its successor.
func (a *App) newPoolGuestImageCommand() *cobra.Command {
	var source string
	var restart bool
	cmd := &cobra.Command{
		Use:     "build-guest [POOL_ID]",
		Short:   "Rebuild the VM image a pool's backend boots, on that pool's own host",
		Aliases: []string{"build-vm-image"},
		Long: `Rebuild the guest VM image from a local checkout, using the pool's own Docker.

The build runs on the Docker daemon inside the pool's VM and the artifacts — the
kernel, the initrd, and the root filesystem — are streamed back to this machine,
into the directory the provider prefers over the published image. That is what
makes it work on a Mac, which has no Docker daemon of its own: the guest that is
already running builds the one that replaces it.

The pool has to be up enough to run a build. A running VM boots the artifacts it
was started with, so nothing adopts the new guest until the machine is replaced:
pass --restart to stop the pool's host once the build lands, and its reconcile
starts it again on the new image. That keeps the pool's disks — its images,
volumes and containers — and stops whatever was running on it.

Without POOL_ID the project's default pool is used.`,
		Example: `  discobox admin pool build-guest
  discobox admin pool build-guest --restart
  discobox admin pool build-guest pool_01hq --source ~/src/discobox`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			apiClient, err := a.apiClient()
			if err != nil {
				return err
			}
			var poolID string
			if len(args) > 0 {
				poolID, err = a.resolvePoolID(cmd.Context(), apiClient, projectID, args[0])
			} else {
				poolID, err = a.defaultPoolID(cmd.Context(), apiClient, projectID)
				if err == nil && strings.TrimSpace(poolID) == "" {
					err = errors.New("no default pool for this project; pass a pool ID")
				}
			}
			if err != nil {
				return err
			}
			resolved, err := guestImageSource(cmd.Context(), source)
			if err != nil {
				return err
			}
			return a.streamGuestImageBuild(cmd.Context(), projectID, poolID, resolved, restart, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Checkout to build the guest image from (default: the repository this is run in)")
	cmd.Flags().BoolVar(&restart, "restart", false, "Stop the pool's host when the build lands, so it comes back on the new image")
	return cmd
}

// guestImageSource resolves the checkout to build from. It defaults to the
// repository the command was run in, which is where a developer editing the
// guest image already is; the server reads it directly, so it is sent as an
// absolute path rather than as anything this machine has to interpret.
func guestImageSource(ctx context.Context, configured string) (string, error) {
	if dir := strings.TrimSpace(configured); dir != "" {
		// Absolute, because the server resolves it on its own filesystem, and
		// a relative path would silently mean a different directory there.
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve --source %q: %w", dir, err)
		}
		return absolute, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gitutil.Root(ctx, cwd)
	if err != nil || strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%s is not in a Git repository, so there is no checkout to build the guest image from; pass --source", cwd)
	}
	return root, nil
}

// streamGuestImageBuild runs the build and prints it as it happens. The build's
// own output goes to stdout; everything this command has to say about it goes
// to stderr, so redirecting the build log captures the build and nothing else.
func (a *App) streamGuestImageBuild(ctx context.Context, projectID, poolID, source string, restart bool, stdout, stderr io.Writer) error {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) + "/guest-image"
	query := url.Values{"source": []string{source}}
	if restart {
		query.Set("restart", "true")
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("build the guest image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if message := attachErrorMessage(body); message != "" {
			return fmt.Errorf("build the guest image: %s", message)
		}
		return fmt.Errorf("build the guest image: %s", resp.Status)
	}
	destination := strings.TrimSpace(resp.Header.Get(guestImageDestinationHeader))
	fmt.Fprintf(stderr, "Building the guest image from %s on pool %s\n", source, poolID)

	if _, err := io.Copy(stdout, resp.Body); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("build the guest image: %w", err)
	}
	// Read after the body, which is when Go fills the trailer in. It is set
	// only for a build that failed.
	if failure := strings.TrimSpace(resp.Trailer.Get(guestImageErrorTrailer)); failure != "" {
		return fmt.Errorf("build the guest image: %s", failure)
	}
	if destination == "" {
		return nil
	}
	fmt.Fprintf(stderr, "\nGuest image written to %s\n", destination)
	if restart {
		fmt.Fprintf(stderr, "The pool host was stopped; its reconcile starts it again on this image.\n")
		return nil
	}
	// Said in full rather than as advice: a running VM keeps the artifacts it
	// booted with, so a build with nothing after it changes nothing anybody can
	// see, which is indistinguishable from a build that failed silently.
	fmt.Fprintf(stderr, "Pool %s is still running the guest it booted with. Re-run with --restart to boot this one.\n", poolID)
	return nil
}
