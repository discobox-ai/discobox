package files

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// HelperCommand is the argument that makes this binary the helper.
const HelperCommand = "files-helper"

// The helper's protocol, on its stdin and stdout:
//
//   - The agent writes the Request as one line of JSON. A write's content
//     follows in frames, each a big-endian uint32 length and that many bytes,
//     and a frame of length zero ends it. Content that stops before that
//     frame was cut short, and the write is not made.
//   - The helper answers one line of JSON, a response. A read's content
//     follows it, to the end of the stream.
//
// Framing the content rather than ending it at EOF is what lets a body cut
// short be told from a whole one: either way the helper's stdin closes.
type response struct {
	Result Result `json:"result"`
	Error  *Error `json:"error,omitempty"`
}

const maxFrame = 64 << 10

// RunHelper is the helper: one operation, read from stdin and answered on
// stdout. It runs as the user the agent started it as, which is the whole
// point of it. The operation's own failure is answered, not exited with; a
// non-zero exit means the protocol itself failed.
func RunHelper(stdin io.Reader, stdout io.Writer) int {
	in := bufio.NewReader(stdin)
	line, err := in.ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "files-helper: read request:", err)
		return 2
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		fmt.Fprintln(os.Stderr, "files-helper: decode request:", err)
		return 2
	}
	var body io.Reader = bytes.NewReader(nil)
	if req.Op == OpWrite {
		body = &frameReader{r: in}
	}
	result, content, err := Do(req, body)
	if content != nil {
		defer content.Close()
	}
	var answer response
	answer.Result = result
	if err != nil {
		answer.Error = asError(err)
	}
	out := bufio.NewWriter(stdout)
	encoded, err := json.Marshal(answer)
	if err != nil {
		fmt.Fprintln(os.Stderr, "files-helper: encode response:", err)
		return 2
	}
	_, _ = out.Write(append(encoded, '\n'))
	if content != nil && answer.Error == nil {
		if _, err := io.Copy(out, content); err != nil {
			// The content is cut short, which the agent's reader sees as the
			// helper's exit status.
			fmt.Fprintln(os.Stderr, "files-helper: read:", err)
			_ = out.Flush()
			return 1
		}
	}
	if err := out.Flush(); err != nil {
		return 1
	}
	return 0
}

func asError(err error) *Error {
	var known *Error
	if errors.As(classify(err), &known) {
		return known
	}
	return &Error{Kind: KindInternal, Message: err.Error()}
}

// frameReader reads a write's framed content, and reports content that ends
// before its last frame as io.ErrUnexpectedEOF.
type frameReader struct {
	r       *bufio.Reader
	left    uint32
	done    bool
	scratch [4]byte
}

func (f *frameReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	if f.left == 0 {
		if _, err := io.ReadFull(f.r, f.scratch[:]); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		f.left = binary.BigEndian.Uint32(f.scratch[:])
		if f.left == 0 {
			f.done = true
			return 0, io.EOF
		}
	}
	if uint32(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= uint32(n)
	if errors.Is(err, io.EOF) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// writeFrames sends body to the helper in frames, and the frame that ends it
// only once body has ended without error.
func writeFrames(w io.Writer, body io.Reader) error {
	buf := make([]byte, 4+maxFrame)
	for {
		n, err := body.Read(buf[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(buf[:4], uint32(n))
			if _, werr := w.Write(buf[:4+n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			binary.BigEndian.PutUint32(buf[:4], 0)
			_, werr := w.Write(buf[:4])
			return werr
		}
		if err != nil {
			return err
		}
	}
}

// helper starts the helper for one operation.
type helper struct {
	// argv is the command line that runs RunHelper.
	argv []string
}

// do runs req in a helper started with attr, which carries the user's
// credential. A read's content streams from the helper until the returned
// reader is closed, which also ends the helper.
func (h helper) do(ctx context.Context, attr *syscall.SysProcAttr, req Request, body io.Reader) (Result, io.ReadCloser, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return Result{}, nil, err
	}
	cmd := exec.CommandContext(ctx, h.argv[0], h.argv[1:]...)
	cmd.SysProcAttr = attr
	// The agent's own directory may be one the user cannot enter; the paths
	// are absolute.
	cmd.Dir = "/"
	cmd.Env = []string{}
	stderr := &cappedBuffer{limit: 4 << 10}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, nil, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, nil, fmt.Errorf("start the files helper: %w", err)
	}
	sent := make(chan error, 1)
	go func() {
		err := func() error {
			if _, err := stdin.Write(append(encoded, '\n')); err != nil {
				return err
			}
			if req.Op == OpWrite {
				return writeFrames(stdin, body)
			}
			return nil
		}()
		_ = stdin.Close()
		sent <- err
	}()
	out := bufio.NewReader(stdout)
	line, readErr := out.ReadBytes('\n')
	var answer response
	if readErr == nil {
		readErr = json.Unmarshal(line, &answer)
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		<-sent
		return Result{}, nil, fmt.Errorf("the files helper did not answer: %v (%v) %s", readErr, waitErr, strings.TrimSpace(stderr.String()))
	}
	if answer.Error != nil || req.Op != OpRead {
		_, _ = io.Copy(io.Discard, out)
		waitErr := cmd.Wait()
		sendErr := <-sent
		if answer.Error != nil {
			return Result{}, nil, answer.Error
		}
		if sendErr != nil {
			return Result{}, nil, sendErr
		}
		if waitErr != nil {
			return Result{}, nil, fmt.Errorf("the files helper failed: %v %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		return answer.Result, nil, nil
	}
	<-sent
	return answer.Result, &helperContent{r: out, cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// helperContent is a read's content as the helper streams it.
type helperContent struct {
	r      io.Reader
	cmd    *exec.Cmd
	stdout io.Closer
	stderr *cappedBuffer
	once   sync.Once
	err    error
}

func (c *helperContent) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		// The end of the stream is the end of the content only if the
		// helper finished: one that failed partway closed it too.
		if waitErr := c.wait(); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

func (c *helperContent) Close() error {
	// Closing before the end leaves the helper writing into a closed pipe,
	// which ends it.
	_ = c.stdout.Close()
	_ = c.wait()
	return nil
}

func (c *helperContent) wait() error {
	c.once.Do(func() {
		if err := c.cmd.Wait(); err != nil {
			c.err = fmt.Errorf("the files helper failed partway: %v %s", err, strings.TrimSpace(c.stderr.String()))
		}
	})
	return c.err
}

// cappedBuffer keeps the first limit bytes written to it: enough of a
// helper's stderr to say why it failed.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
