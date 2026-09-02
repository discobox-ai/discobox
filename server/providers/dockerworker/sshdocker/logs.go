package sshdocker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// remoteStopWait bounds how long closing a remote log stream waits for the SSH
// session to finish after the connection has been torn down.
const remoteStopWait = 2 * time.Second

// StreamCommand runs one command on the VM and streams its combined output as
// a pool log.
//
// A cloud VM's host log is not a file on the control plane's disk — there is no
// hypervisor here to have written one — so it is read the only way a VM this
// process did not boot can be read: over the connection the driver already uses
// to reach its Docker daemon. stderr is merged in, because the tool being run
// explains an empty result there.
//
// The stream owns the SSH connection: closing it ends the remote command and
// the connection with it, which is what stops a --follow read.
func (d *Dialer) StreamCommand(ctx context.Context, target Target, command []string, source string) (*sandbox.PoolLogStream, error) {
	client, err := d.Dial(ctx, target)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("open ssh session on %s: %w", target.Host, err)
	}
	reader, writer := io.Pipe()
	session.Stdout = writer
	session.Stderr = writer
	if err := session.Start(strings.Join(command, " ")); err != nil {
		_ = session.Close()
		_ = client.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("run %q on %s: %w", command[0], target.Host, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A non-zero exit has already said what it had to say on the merged
		// stderr, so it ends the stream rather than replacing what was read.
		_ = session.Wait()
		_ = writer.Close()
	}()
	return &sandbox.PoolLogStream{
		Source:     source,
		ReadCloser: &sshStream{reader: reader, session: session, client: client, done: done},
	}, nil
}

type sshStream struct {
	reader  *io.PipeReader
	session *ssh.Session
	client  *ssh.Client
	done    chan struct{}
}

func (s *sshStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *sshStream) Close() error {
	// Closing the read half first unblocks the copy into it, so the wait below
	// cannot hang on a remote command still producing output.
	_ = s.reader.Close()
	_ = s.session.Close()
	err := s.client.Close()
	select {
	case <-s.done:
	case <-time.After(remoteStopWait):
		// The remote side did not finish closing the session down. The caller
		// is usually a request handler whose client has already left, and the
		// connection is closed either way, so this returns rather than waiting
		// on a droplet that has stopped answering.
	}
	return err
}
