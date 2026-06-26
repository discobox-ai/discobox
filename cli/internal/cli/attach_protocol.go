package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

const (
	attachFrameOutput     byte = 1
	attachFrameInput      byte = 2
	attachFrameResize     byte = 3
	attachFrameSignal     byte = 4
	attachFrameError      byte = 5
	attachFrameExit       byte = 6
	attachFrameCloseInput byte = 7

	attachFrameMaxPayload = 16 * 1024 * 1024
)

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("process exited with code %d", e.code)
}

func (e exitCodeError) ExitCode() int {
	return e.code
}

func ExitCode(err error) (int, bool) {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if code < 0 {
			code = 1
		}
		if code > 255 {
			code = 255
		}
		return code, true
	}
	return 0, false
}

type attachExitPayload struct {
	Status   string `json:"status"`
	ExitCode *int64 `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

func attachExitErrorFromPayload(kind string, payload []byte) error {
	var exit attachExitPayload
	if err := json.Unmarshal(payload, &exit); err != nil {
		return fmt.Errorf("decode %s exit frame: %w", kind, err)
	}
	if exit.ExitCode != nil && *exit.ExitCode != 0 {
		return exitCodeError{code: int(*exit.ExitCode)}
	}
	if strings.EqualFold(exit.Status, "failed") {
		if strings.TrimSpace(exit.Error) == "" {
			return fmt.Errorf("%s failed", kind)
		}
		return fmt.Errorf("%s failed: %s", kind, exit.Error)
	}
	return nil
}

func isAttachDone(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "closed network connection") ||
		strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "connection reset by peer")
}
