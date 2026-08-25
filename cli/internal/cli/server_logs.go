package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/endpoint"
)

// followInterval is how often a followed log is checked for new output. A
// server writes in bursts around a request, so polling is enough and costs
// nothing that a watch on every platform would not cost more.
const followInterval = 300 * time.Millisecond

// A server this CLI launched has no terminal to print to, so its output is only
// ever read back out of the file the launch pointed it at. This is the command
// that reads it: `discobox admin server logs`, next to the command that runs the
// server, because the two describe the same process.
func (a *App) newServerLogsCommand() *cobra.Command {
	var (
		follow   bool
		tail     int
		previous bool
		pathOnly bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the output of the locally launched Discobox server",
		Long: "Show the output of the Discobox server running on this machine.\n\n" +
			"The server the CLI starts in the background writes to a file rather than a\n" +
			"terminal. Every launch appends to that file behind a banner line, so the run\n" +
			"that failed is still there after the restart that followed it; once the file\n" +
			"grows past its limit it rotates, and --previous reads what was rotated out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := endpoint.ServerLogPath()
			if previous {
				path = endpoint.PreviousServerLogPath(path)
			}
			if pathOnly {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), path)
				return err
			}
			return showServerLog(cmd.Context(), cmd.OutOrStdout(), path, tail, follow && !previous)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing the log as the server writes to it")
	cmd.Flags().IntVar(&tail, "tail", 0, "Print only the last N lines; 0 prints the whole log")
	cmd.Flags().BoolVar(&previous, "previous", false, "Read the log rotated out of the way of the current one")
	cmd.Flags().BoolVar(&pathOnly, "path", false, "Print where the log is instead of what is in it")
	return cmd
}

// showServerLog writes the log to out, and keeps writing it when following.
func showServerLog(ctx context.Context, out io.Writer, path string, tail int, follow bool) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no server log at %s: no server has been started from this machine yet", path)
	} else if err != nil {
		return err
	}
	defer file.Close()

	offset, err := writeLogStart(out, file, tail)
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followInterval):
		}
		info, err := file.Stat()
		if err != nil {
			return err
		}
		// A rotation replaces the file under us, and the old handle would then
		// stay silent forever. A shorter file than the offset already read is
		// how that shows up from here, so start the new one over.
		if info.Size() < offset {
			offset = 0
		}
		if info.Size() == offset {
			continue
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		written, err := io.Copy(out, file)
		if err != nil {
			return err
		}
		offset += written
	}
}

// writeLogStart writes what the log already holds and reports the offset the
// rest of it will arrive at.
func writeLogStart(out io.Writer, file *os.File, tail int) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if tail <= 0 {
		written, err := io.Copy(out, file)
		return written, err
	}
	// The last N lines, read from the end rather than by scanning a log that
	// may be megabytes of a server's whole history.
	const window = 1 << 20
	size := min(info.Size(), int64(window))
	data := make([]byte, size)
	if size > 0 {
		if _, err := file.ReadAt(data, info.Size()-size); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if len(data) > 0 {
		if _, err := fmt.Fprintln(out, strings.Join(lines, "\n")); err != nil {
			return 0, err
		}
	}
	return info.Size(), nil
}
