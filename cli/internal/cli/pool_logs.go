package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// poolLogSourceHeader carries what the backend actually opened, since there is
// no uniform pool host log and the bytes alone do not say which record they
// came from.
const poolLogSourceHeader = "X-Discobox-Pool-Log-Source"

// defaultPoolLogTail bounds an unasked-for read. A guest console log spans
// every boot the pool has ever had, and the operator running this almost always
// means the most recent one.
const defaultPoolLogTail = 200

// newPoolLogsCommand implements `discobox admin pool logs`: whatever the pool's
// backend recorded about the machine hosting its runtime.
//
// There is no single thing that is: a VM backend has its guest's serial
// console, the local Docker backend has the daemon's systemd journal, and a
// backend driven by a script has whatever the script prints. The command asks
// the driver and labels what it got.
func (a *App) newPoolLogsCommand() *cobra.Command {
	var tail int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [POOL_ID]",
		Short: "Print the host log for a pool's backend",
		Long: `Print what a pool's backend recorded about the machine hosting its runtime.

Each backend keeps a different record, and this prints whichever one the pool's
driver has:

  vz, libkrun     the guest VM's serial console, including the boot that failed
  docker          the Docker daemon's systemd journal on the machine running it
  digitalocean    the droplet's Docker daemon journal, read over SSH
  wslc            the guest's journal, or its kernel ring buffer
  exec            whatever the configured command prints for its logs operation

The log is read through the provider driver rather than the pool agent, so it
answers on a host whose agent never registered — which is when a host log is
worth reading. A backend that keeps no readable record says so instead of
printing nothing.

Without POOL_ID the project's default pool is used.`,
		Example: `  discobox admin pool logs
  discobox admin pool logs --follow
  discobox admin pool logs pool_01hq --tail 50`,
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
			return a.streamPoolLogs(cmd.Context(), projectID, poolID, tail, follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().IntVar(&tail, "tail", defaultPoolLogTail, "Print only the last N lines (0 for the whole log)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing as the host writes")
	return cmd
}

// streamPoolLogs copies the host log to stdout, naming its source on stderr so
// a redirected `discobox admin pool logs > console.txt` captures the log and
// nothing else.
func (a *App) streamPoolLogs(ctx context.Context, projectID, poolID string, tail int, follow bool, stdout, stderr io.Writer) error {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) + "/logs"
	query := url.Values{}
	if tail > 0 {
		query.Set("tail", strconv.Itoa(tail))
	}
	if follow {
		query.Set("follow", "true")
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("read pool host logs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if message := attachErrorMessage(body); message != "" {
			return fmt.Errorf("read pool host logs: %s", message)
		}
		return fmt.Errorf("read pool host logs: %s", resp.Status)
	}
	if source := strings.TrimSpace(resp.Header.Get(poolLogSourceHeader)); source != "" {
		fmt.Fprintf(stderr, "Pool %s host log: %s\n", poolID, source)
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		// A followed log ends when the operator interrupts it or the host stops
		// answering; neither is a failure of the command.
		if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return fmt.Errorf("read pool host logs: %w", err)
	}
	return nil
}
