package dockerworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// followPollInterval is how often a followed file is re-read after it runs
// dry. A serial console is written a line at a time by a booting guest, so
// this only has to be faster than an operator reading it.
const followPollInterval = 250 * time.Millisecond

// commandStopWait bounds how long closing a log stream waits for the killed
// command to be reaped. Closing must be prompt: the caller is a handler
// releasing a client's stream, not something that can afford to block.
const commandStopWait = 2 * time.Second

// dockerJournalUnits are the systemd units a host's Docker daemon runs under.
// They are all passed at once rather than probed in turn: journalctl ORs
// repeated -u matches, so naming every packaging of the daemon costs nothing
// and finding the right one needs no guess about how it was installed.
var dockerJournalUnits = []string{"docker.service", "docker.socket", "snap.docker.dockerd.service"}

// OpenLogs returns what the driver recorded about the pool's host.
//
// The engine adds nothing of its own here, deliberately: it owns Docker, and
// the log an operator wants is the one from underneath Docker — the console of
// the VM that would not boot, the journal of the daemon that would not start.
// Only the driver knows where that is.
func (e *Engine) OpenLogs(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	if pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, errors.New("pool is required")
	}
	return e.driver.PoolLogs(ctx, pool.ID, opts)
}

// JournalCommand is the journalctl invocation that reads a host's Docker
// daemon log, for the drivers whose "VM" is a machine running systemd — the
// local host for the docker driver, the guest for the ones that can run a
// command inside theirs.
func JournalCommand(opts sandbox.PoolLogOptions) []string {
	args := []string{"journalctl", "--no-pager"}
	for _, unit := range dockerJournalUnits {
		args = append(args, "-u", unit)
	}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	} else {
		args = append(args, "--no-tail")
	}
	if opts.Follow {
		args = append(args, "-f")
	}
	return args
}

// StreamCommand runs a command and streams its combined output as a pool log.
//
// stderr is merged into the stream rather than being reported separately,
// because the tools this runs explain themselves there: "No journal files were
// found" is the answer to the operator's question, not a transport failure. A
// non-zero exit therefore ends the stream normally — whatever the command had
// to say about it has already been read.
func StreamCommand(cmd *exec.Cmd, source string) (*sandbox.PoolLogStream, error) {
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	// Its own process group, so stopping it stops what it started. A log
	// command is routinely a shell around the tool that produces the output —
	// the exec driver's whole contract is "run this script" — and killing only
	// the shell leaves that child alive holding the write end of this stream.
	group := newProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("read pool host logs with %s: %w", cmd.Path, err)
	}
	group.adopt()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
		_ = writer.Close()
	}()
	return &sandbox.PoolLogStream{
		Source:     source,
		ReadCloser: &commandStream{reader: reader, group: group, done: done},
	}, nil
}

// commandStream is one running log command. Closing it kills the command,
// which is the only way a --follow read ever ends.
type commandStream struct {
	reader *io.PipeReader
	group  *processGroup
	done   chan struct{}
}

func (s *commandStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *commandStream) Close() error {
	s.group.kill()
	// Closing the read half is what unblocks a write the kill did not beat, so
	// the wait below cannot hang on a command holding a full pipe.
	_ = s.reader.Close()
	select {
	case <-s.done:
	case <-time.After(commandStopWait):
		// Something the kill could not reach still holds the command's output:
		// a process that ignores signals, or a grandchild in another group. It
		// is reaped by the goroutine whenever it does end, and until then this
		// must not hold up its caller, which is usually a request handler whose
		// client has already gone.
	}
	return nil
}

// TailFile opens a file a backend appends its host log to — a VM's serial
// console — positioned at the last opts.Tail lines, following the file as it
// grows when asked.
//
// Following polls rather than watching: these files are written by a hypervisor
// on this machine, an inotify watch would still have to handle the file being
// truncated or replaced between boots, and a quarter second of latency is
// invisible to someone reading a boot log.
func TailFile(ctx context.Context, path string, opts sandbox.PoolLogOptions) (io.ReadCloser, error) {
	file, err := os.Open(path) //nolint:gosec // The path is composed by the driver from its own state directory.
	if err != nil {
		return nil, err
	}
	if opts.Tail > 0 {
		if err := seekToTail(file, opts.Tail); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if !opts.Follow {
		return file, nil
	}
	return &followReader{ctx: ctx, file: file}, nil
}

// seekToTail positions the file at the start of its last n lines, scanning
// backwards from the end so a console log that has been running for a week
// costs one read of its tail rather than a read of the whole file.
func seekToTail(file *os.File, n int) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	offset := info.Size()
	// The newline ending the last line is not a line separator for this
	// purpose; counting past it is what makes -n 1 mean "the last line".
	seen := 0
	for offset > 0 {
		size := int64(chunkSize)
		if offset < size {
			size = offset
		}
		offset -= size
		if _, err := file.ReadAt(buf[:size], offset); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for i := int(size) - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			if offset+int64(i)+1 == info.Size() {
				continue
			}
			seen++
			if seen == n {
				_, err := file.Seek(offset+int64(i)+1, io.SeekStart)
				return err
			}
		}
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

// followReader reads a growing file, waiting at EOF instead of reporting it.
// The wait ends when the caller's context does, which is how a client that
// disconnects releases the file.
type followReader struct {
	ctx  context.Context
	file *os.File
}

func (r *followReader) Read(p []byte) (int, error) {
	for {
		n, err := r.file.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		select {
		case <-r.ctx.Done():
			// The caller went away; this is the end of the stream, not a
			// failure to read one.
			return 0, io.EOF
		case <-time.After(followPollInterval):
		}
	}
}

func (r *followReader) Close() error { return r.file.Close() }
