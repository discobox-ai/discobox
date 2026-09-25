// Package refreshcmd runs the command a token suggests for its own renewal, on
// the machine of the person who agreed to run it (ADR 26-09-25-122 §5).
//
// The command is an argument vector a person saw before it ran: it is executed
// directly, never through a shell, with no stdin, the caller's environment, a
// deadline, and a bound on what it may print. What it prints, trimmed, is the
// value. Nothing here decides whether to run it; the launcher's dialog and the
// `secret refresh` command do.
package refreshcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	// Timeout bounds a command: long enough for a login helper to reach its
	// keychain or exchange a token, short enough that a hung one is reported.
	Timeout = 30 * time.Second
	// MaxOutput is the most a command may print. A credential is a line; a
	// command printing more than this is not printing one.
	MaxOutput = 64 << 10
)

// Run runs command and returns what it printed, trimmed. An empty result, a
// non-zero exit, output past MaxOutput, and a command still running at Timeout
// are failures; a failure's message carries the first line of stderr.
func Run(ctx context.Context, command []string) (string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "", errors.New("no command to run")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	//nolint:gosec // G204: the command is one a person saw and chose to run.
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin = nil
	stdout := &limitedBuffer{limit: MaxOutput}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: MaxOutput}
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s did not finish within %s", command[0], Timeout)
	}
	if err != nil {
		if line := firstLine(stderr.String()); line != "" {
			return "", fmt.Errorf("%s: %s", command[0], line)
		}
		return "", fmt.Errorf("%s: %w", command[0], err)
	}
	if stdout.overflow {
		return "", fmt.Errorf("%s printed more than %d bytes; a credential is one line", command[0], MaxOutput)
	}
	value := strings.TrimSpace(stdout.String())
	if value == "" {
		return "", fmt.Errorf("%s printed nothing", command[0])
	}
	return value, nil
}

// Split reads a command as a person types it: words separated by spaces, with
// single quotes, double quotes, and backslashes to keep a space inside one.
// Nothing is expanded; it is an argument vector, not a shell line.
func Split(line string) ([]string, error) {
	var (
		args    []string
		word    strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range line {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape in command")
	}
	if inWord {
		args = append(args, word.String())
	}
	return args, nil
}

// Join writes command the way Split reads it, quoting only the arguments that
// need it, so a person reads exactly what will run.
func Join(command []string) string {
	parts := make([]string, len(command))
	for i, arg := range command {
		if arg != "" && !strings.ContainsAny(arg, " \t\n'\"\\") {
			parts[i] = arg
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// limitedBuffer keeps up to limit bytes and notes that more arrived, without
// failing the write: a command that prints too much is allowed to finish and
// is then refused, rather than dying on a broken pipe.
type limitedBuffer struct {
	buf      *bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf == nil {
		b.buf = &bytes.Buffer{}
	}
	room := b.limit - b.buf.Len()
	if room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	if b.buf == nil {
		return ""
	}
	return b.buf.String()
}

var _ io.Writer = (*limitedBuffer)(nil)
