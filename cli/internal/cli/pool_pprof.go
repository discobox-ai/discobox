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
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// poolPprofProcesses are the pool processes a profile can be taken of.
var poolPprofProcesses = []string{"agent", "proxy", "mediator"}

// newPoolPprofCommand implements `discobox admin pool pprof`: one profile of
// one of a pool's processes, fetched through the pool's agent and written to
// a file for `go tool pprof`.
func (a *App) newPoolPprofCommand() *cobra.Command {
	var process, profile, output string
	var seconds int
	cmd := &cobra.Command{
		Use:   "pprof [POOL] --process agent|proxy|mediator",
		Short: "Write a Go profile of one of a pool's processes to a file",
		Long: `Take a Go pprof profile of one of a pool's processes and write it to a file.

A pool runs three long-lived processes: its agent, its proxy (egress and DNS),
and its BuildKit mediator. On a pool with profiling on (pool create or update
--profiling), each serves Go's profiles on the pool's own loopback, and the
pool's agent fetches the one asked for. Nothing on the pool is reached any
other way, and nothing is kept on the server.

--profile names what to take:

  profile       CPU, sampled for --seconds (default 30)
  trace         execution trace, for --seconds (default 1)
  heap          live heap
  allocs        every allocation since start
  goroutine     every goroutine's stack
  block         where goroutines block on synchronization
  mutex         where mutex holders keep others waiting
  threadcreate  where OS threads are created

The file opens with ` + "`go tool pprof FILE`" + `, or a trace with ` + "`go tool trace FILE`" + `.

Without POOL the project's default pool is used.`,
		Example: `  discobox admin pool pprof --process proxy
  discobox admin pool pprof --process agent --profile heap -f agent-heap.pb.gz
  discobox admin pool pprof pool_01hq --process mediator --profile goroutine`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(poolPprofProcesses, process) {
				return fmt.Errorf("--process must be one of %s", strings.Join(poolPprofProcesses, ", "))
			}
			if cmd.Flags().Changed("seconds") && seconds <= 0 {
				return errors.New("--seconds must be greater than 0")
			}
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
			if output == "" {
				output = defaultPoolPprofFile(process, profile)
			}
			return a.fetchPoolPprof(cmd.Context(), projectID, poolID, process, profile, seconds, output, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&process, "process", "", "The process to profile: agent, proxy or mediator")
	cmd.Flags().StringVar(&profile, "profile", "profile", "What to take: profile (CPU), trace, heap, allocs, goroutine, block, mutex or threadcreate")
	cmd.Flags().IntVar(&seconds, "seconds", 0, "How long a CPU profile or trace samples (the process's default when unset: 30 for a CPU profile, 1 for a trace)")
	// Not --output: the root's --output is the format every command prints in.
	cmd.Flags().StringVarP(&output, "file", "f", "", "File to write the profile to, or - for stdout (default <process>-<profile>.pb.gz, or <process>.trace for a trace)")
	_ = cmd.MarkFlagRequired("process")
	_ = cmd.RegisterFlagCompletionFunc("process", cobra.FixedCompletions(poolPprofProcesses, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// defaultPoolPprofFile names a profile written without --file: a pprof profile is
// a gzipped protobuf, a trace is not.
func defaultPoolPprofFile(process, profile string) string {
	if profile == "trace" {
		return process + ".trace"
	}
	return process + "-" + profile + ".pb.gz"
}

// fetchPoolPprof writes one profile to output, or to stdout for "-". A file is
// written beside its name and renamed into place once whole, so a profile cut
// short leaves nothing that reads as one.
func (a *App) fetchPoolPprof(ctx context.Context, projectID, poolID, process, profile string, seconds int, output string, stdout, stderr io.Writer) error {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) +
		"/pprof/" + url.PathEscape(process) + "/" + url.PathEscape(profile)
	if seconds > 0 {
		u.RawQuery = url.Values{"seconds": []string{strconv.Itoa(seconds)}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if profile == "profile" || profile == "trace" {
		taking := seconds
		if taking == 0 {
			taking = map[string]int{"profile": 30, "trace": 1}[profile]
		}
		fmt.Fprintf(stderr, "Taking %s's %s for %ds on pool %s...\n", process, profile, taking, poolID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// The server's own refusals are its JSON error envelope; a plain 404
		// is the router of a server from before the route. The pool's agent
		// predating it is the server's 503, which says so.
		var plain *plainStatusError
		if errors.As(err, &plain) && plain.code == http.StatusNotFound {
			return fmt.Errorf("profile pool %s: the server predates profiling a pool's processes; upgrade it", poolID)
		}
		return fmt.Errorf("profile pool %s: %w", poolID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if message := attachErrorMessage(body); message != "" {
			return fmt.Errorf("profile pool %s: %s", poolID, message)
		}
		return fmt.Errorf("profile pool %s: %s", poolID, resp.Status)
	}
	if output == "-" {
		if _, err := io.Copy(stdout, resp.Body); err != nil {
			return fmt.Errorf("profile pool %s: %w", poolID, err)
		}
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(output), "."+filepath.Base(output)+".*")
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(file.Name())
		return fmt.Errorf("profile pool %s: %w", poolID, err)
	}
	if err := os.Rename(file.Name(), output); err != nil {
		_ = os.Remove(file.Name())
		return err
	}
	tool := "pprof"
	if profile == "trace" {
		tool = "trace"
	}
	fmt.Fprintf(stderr, "Wrote %s; open it with: go tool %s %s\n", output, tool, output)
	return nil
}
