package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/controlplane"
	"github.com/discobox-ai/discobox/execstream/client"
	"github.com/discobox-ai/discobox/serverstage"
)

func (a *App) newServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Discobox API server",
		Long: `Run the Discobox API server in the foreground.

The server is a separate program, and this is the command that gets one: it
stages the server this build was cut against if the machine does not have it
already, then runs it, passing through its output and its exit status.

A server binary sitting next to this one is used as it is, which is what a
development build runs. "discobox admin server stage" does the download half on
its own.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			progress := a.serverStartupLine()
			path, err := a.resolveServer(cmd.Context(), func(report serverstage.Progress) {
				progress.set(serverStageText(report))
			})
			progress.clear()
			if err != nil {
				return err
			}
			return runServerProcess(cmd, path)
		},
	}
	cmd.Flags().StringVar(&a.serverSource.binary, "binary", "",
		"Run this server binary instead of resolving one (also "+ServerBinaryEnv+")")
	cmd.Flags().StringVar(&a.serverSource.manifest, "manifest", "",
		"Stage from this manifest file or URL instead of the one this build carries (also "+ServerManifestEnv+")")
	cmd.AddCommand(a.newServerStageCommand())
	cmd.AddCommand(a.newServerManifestCommand())
	cmd.AddCommand(a.newServerShutdownCommand())
	cmd.AddCommand(a.newServerLogsCommand())
	return cmd
}

// runServerProcess runs the server in the foreground, as the thing this command
// stands in for.
//
// No signal handling: the child is in this process's group, so the terminal's
// interrupt reaches it directly, and forwarding one would deliver it twice. The
// context is what ends the server otherwise — it is canceled when the process
// that asked for this one goes away (watchParentProcess), and a server outliving
// the window it was started in is not what anybody meant.
func runServerProcess(cmd *cobra.Command, path string) error {
	//nolint:gosec // The path is the resolved server binary; see serverResolver.
	server := exec.CommandContext(cmd.Context(), path)
	server.Stdin = cmd.InOrStdin()
	server.Stdout = cmd.OutOrStdout()
	server.Stderr = cmd.ErrOrStderr()
	if err := server.Run(); err != nil {
		var exit *exec.ExitError
		// The server's status is this command's status, silently: a server that
		// stopped has already said why, on the stderr passed through above, and
		// a second sentence about an exit code would be the CLI's own noise.
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			return client.ExitError{Code: exit.ExitCode()}
		}
		return fmt.Errorf("run %s: %w", path, err)
	}
	return nil
}

func (a *App) newServerStageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stage",
		Short: "Download the server this build runs and check it against the digests it was cut with",
		Long: `Download the Discobox server and verify it.

A release CLI carries a description of the server it was cut against — where
each of its files lives and what each one's SHA-256 is — and this turns that
description into files on this machine, under the state directory, one
directory per server version. An asset whose digest does not match is not kept.

Nothing has to run this: a command that needs a server stages one on its own.
It is here so the download can be done deliberately — while there is a network,
while an image is being built, before a machine is handed to somebody — rather
than in front of the first command that wanted a server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.stageServer(cmd)
		},
	}
	cmd.Flags().StringVar(&a.serverSource.manifest, "manifest", "",
		"Stage from this manifest file or URL instead of the one this build carries (also "+ServerManifestEnv+")")
	cmd.Flags().BoolVar(&a.serverSource.force, "force", false,
		"Download and verify again even when this version is already staged")
	return cmd
}

func (a *App) stageServer(cmd *cobra.Command) error {
	resolver := a.serverResolver(nil)
	manifest, err := resolver.manifest(cmd.Context())
	if err != nil {
		return err
	}
	// Whether this call is what fetched it, asked before it happens, so the
	// report can say "already staged" rather than claiming a download that was
	// four stat calls.
	_, already := serverstage.Staged(resolver.stageRoot, manifest)
	progress := a.serverStartupLine()
	resolver.onProgress = func(report serverstage.Progress) {
		progress.set(serverStageText(report))
	}
	dir, err := resolver.stage(cmd.Context(), manifest)
	progress.clear()
	if err != nil {
		return err
	}

	if a.output == "json" {
		assets := make([]map[string]any, 0, len(manifest.Assets))
		for _, asset := range manifest.Assets {
			assets = append(assets, map[string]any{
				"name":   asset.Name,
				"path":   filepath.Join(dir, asset.Name),
				"sha256": asset.SHA256,
				"url":    asset.URL,
			})
		}
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"version":   manifest.Version,
			"os":        manifest.OS,
			"arch":      manifest.Arch,
			"directory": dir,
			"command":   filepath.Join(dir, manifest.Command),
			"assets":    assets,
			"staged":    !already || a.serverSource.force,
		})
	}
	out := cmd.OutOrStdout()
	if a.quiet {
		_, err := fmt.Fprintln(out, filepath.Join(dir, manifest.Command))
		return err
	}
	what := "staged"
	if already && !a.serverSource.force {
		what = "already staged"
	}
	fmt.Fprintf(out, "%s discobox server %s for %s\n", what, manifest.Version, manifest.Platform())
	for _, asset := range manifest.Assets {
		fmt.Fprintf(out, "  %s\n", filepath.Join(dir, asset.Name))
	}
	return nil
}

func (a *App) newServerManifestCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Print the description of the server this build would download",
		Long: `Print the server manifest this build carries.

It says which files the server is made of, where each one is published, and
what each one's SHA-256 has to be. A build with no manifest — every build that
is not a release — says so and exits non-zero.

This is the answer to "what would this binary download, and from where", which
is otherwise only visible as a directory of files after the fact.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manifest, err := a.serverResolver(nil).manifest(cmd.Context())
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), manifest)
		},
	}
	cmd.Flags().StringVar(&a.serverSource.manifest, "manifest", "",
		"Read this manifest file or URL instead of the one this build carries (also "+ServerManifestEnv+")")
	return cmd
}

// serverStageText says what a staging is doing, for the same one-line status
// the launch itself narrates on.
func serverStageText(report serverstage.Progress) string {
	if report.Done {
		return "staging discobox server · done"
	}
	var b strings.Builder
	b.WriteString("downloading ")
	b.WriteString(report.Asset)
	if report.Assets > 1 {
		fmt.Fprintf(&b, " (%d of %d)", report.Index, report.Assets)
	}
	switch {
	case report.Total > 0:
		fmt.Fprintf(&b, " · %s of %s", humanBytes(report.Current), humanBytes(report.Total))
	case report.Current > 0:
		fmt.Fprintf(&b, " · %s", humanBytes(report.Current))
	}
	return b.String()
}

func (a *App) newServerShutdownCommand() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "shutdown",
		Short: "Ask the Discobox API server to stop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			baseURL, httpClient, err := a.httpClientWithAutoStart(false)
			if err != nil {
				return err
			}
			resp, err := requestServerShutdown(cmd.Context(), baseURL, httpClient)
			if err != nil && a.serverEndpoint().AutoLaunchable() {
				baseURL, httpClient = defaultHTTPShutdownClient()
				resp, err = requestServerShutdown(cmd.Context(), baseURL, httpClient)
			}
			if err != nil {
				return fmt.Errorf("shutdown server: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("shutdown server: %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			if wait {
				if err := waitForServerShutdown(cmd.Context(), baseURL, httpClient, 10*time.Second); err != nil {
					return err
				}
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"shutdown": true, "wait": wait})
			}
			message := "shutdown requested"
			if wait {
				message = "shutdown complete"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
			return err
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until the server stops accepting requests")
	return cmd
}

func requestServerShutdown(ctx context.Context, baseURL string, httpClient *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/shutdown", nil)
	if err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func defaultHTTPShutdownClient() (string, *http.Client) {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = fmt.Sprint(controlplane.DefaultPort)
	}
	return "http://127.0.0.1:" + port, http.DefaultClient
}

func waitForServerShutdown(ctx context.Context, baseURL string, httpClient *http.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			cancel()
			return err
		}
		resp := doShutdownProbe(httpClient, req)
		cancel()
		if resp == nil {
			return nil
		}
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("shutdown server: still accepting requests after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func doShutdownProbe(httpClient *http.Client, req *http.Request) *http.Response {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	return resp
}
