package agentlayer

import (
	"bytes"
	"encoding/json"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// The snapshot command's standard output is its progress, for the taker
// (ADR 26-11-23-004 §1): one line each time the build's provisioning status
// changes, the whole provisionspec.Status, and nothing else. Each line is
// SnapshotProgressPrefix followed by the status as one line of JSON. The
// prefix tells a progress line from stray output a step's child process
// left on the stream; a taker drops any line it cannot read as one, and the
// build's result stays its exit status and stderr.
const (
	// SnapshotProgressPrefix opens each progress line.
	SnapshotProgressPrefix = "discobox-snapshot-progress "
	// SnapshotProgressLimit bounds a progress line, the newline included: a
	// taker drops a longer one.
	SnapshotProgressLimit = 256 << 10
)

// SnapshotProgressLine is status as the snapshot command writes it.
func SnapshotProgressLine(status provisionspec.Status) ([]byte, error) {
	data, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	line := make([]byte, 0, len(SnapshotProgressPrefix)+len(data)+1)
	line = append(line, SnapshotProgressPrefix...)
	line = append(line, data...)
	return append(line, '\n'), nil
}

// ParseSnapshotProgress reads one line of the snapshot command's output,
// without its newline, as the status it reports, or reports false for a line
// that is not one.
func ParseSnapshotProgress(line []byte) (provisionspec.Status, bool) {
	data, ok := bytes.CutPrefix(line, []byte(SnapshotProgressPrefix))
	if !ok {
		return provisionspec.Status{}, false
	}
	var status provisionspec.Status
	if json.Unmarshal(data, &status) != nil || status.State == "" {
		return provisionspec.Status{}, false
	}
	return status, true
}
