package provisionspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// StatusPath is the box's provisioning state, for this boot (ADR
// 26-10-16-423 §6): written by the sandbox agent, readable by everyone, and
// replaced whole on each change. What has been applied across boots is the
// step records instead.
const StatusPath = "/run/discobox/provision.json"

// State is where provisioning, or one feature, stands.
type State string

const (
	StatePending      State = "pending"
	StateProvisioning State = "provisioning"
	StateReady        State = "ready"
	StateFailed       State = "failed"
	// StateOff is a feature the project opted out of; it is never the
	// document's own state.
	StateOff State = "off"
)

// Done reports whether s is the outcome of a provisioning attempt.
func (s State) Done() bool {
	return s == StateReady || s == StateFailed || s == StateOff
}

// How is where a ready feature came from.
type How string

const (
	// HowInstalled is a feature this boot applied steps of.
	HowInstalled How = "installed"
	// HowAdopted is the box's own copy, found usable and wired.
	HowAdopted How = "adopted"
	// HowSnapshot is a feature every step of which was already in place on
	// the root filesystem the box booted.
	HowSnapshot How = "snapshot"
)

// Status is provision.json.
type Status struct {
	// State is provisioning while any feature is pending or provisioning,
	// failed once all are done and one failed, and ready once all are.
	State State `json:"state"`
	// Ready is whether everything readiness waits for is in place: core,
	// what the harness requires, and every feature's wiring.
	Ready bool `json:"ready"`
	// Error is why readiness failed or provisioning could not start.
	Error     string                   `json:"error,omitempty"`
	UpdatedAt time.Time                `json:"updatedAt"`
	Features  map[string]FeatureStatus `json:"features"`
	Retry     *ScheduledRetry          `json:"retry,omitempty"`
}

// ScheduledRetry describes the next automatic reprovision attempt.
type ScheduledRetry struct {
	Attempt int       `json:"attempt"`
	Of      int       `json:"of"`
	At      time.Time `json:"at"`
}

// FeatureStatus is one feature's line of the document. Every string in it
// was written by the box: a reader outside it escapes them.
type FeatureStatus struct {
	State State `json:"state"`
	How   How   `json:"how,omitempty"`
	// Version is the version the feature's adopt check read.
	Version string `json:"version,omitempty"`
	// Async is a feature provisioned after readiness.
	Async bool `json:"async,omitempty"`
	// Step is the step running now.
	Step string `json:"step,omitempty"`
	// Log is where the feature's steps write their output.
	Log string `json:"log,omitempty"`
	// Reason is why a feature is off.
	Reason string `json:"reason,omitempty"`
	// Error is why a feature failed.
	Error string `json:"error,omitempty"`
	// Progress is how far the step named by Step has got, present only
	// while the feature is provisioning, and reset when its step changes
	// (ADR 26-11-24-336 §1). It is shown, never acted on.
	Progress *Progress `json:"progress,omitempty"`
}

// Progress is how far a running step has got: what the step says it is
// doing, and what the box has received since it started (ADR 26-11-24-336
// §§1–4). Every part of it is the box's word.
type Progress struct {
	// Detail is one line of what the step is doing now, at most
	// MaxProgressDetail bytes.
	Detail string `json:"detail,omitempty"`
	// Done and Total are how far the step has got in bytes, Total only when
	// the step knows it.
	Done  int64 `json:"done,omitempty"`
	Total int64 `json:"total,omitempty"`
	// Percent is how far the step has got when it knows a fraction and no
	// bytes (apt's install phase), 0 to 100.
	Percent int `json:"percent,omitempty"`
	// Received is the bytes the box received since the step started, by
	// every program in it: the box's count, never the step's own. It moves
	// only with traffic above what the pool's own calls into the box make.
	Received int64 `json:"received,omitempty"`
	// ReceivedAt is when Received last moved, or the step started when it
	// has not; zero, with no Received, is a box that cannot count what it
	// receives.
	ReceivedAt time.Time `json:"receivedAt,omitzero"`
}

// ProgressEnv names the file a provisioning step's script reports its
// progress on (ADR 26-11-24-336 §2). It is unset outside the agent's run.
const ProgressEnv = "DISCOBOX_PROVISION_PROGRESS"

// MaxProgressDetail bounds a step's detail line.
const MaxProgressDetail = 256

// ParseProgressLine reads one line a step wrote to its progress file, and
// false when it says nothing. A line is the step's detail, after any of
// these words, in any order:
//
//	bytes=DONE[/TOTAL]   how many bytes of how many it has fetched
//	percent=N            how far it has got, 0 to 100
//
// so `percent=62 installing nodejs`. apt's own status lines
// (`APT::Status-Fd`: `dlstatus:…`, `pmstatus:…`) are read too, so a step
// can point apt at the file as it is. Only the detail, done, total and
// percent of the result are set.
func ParseProgressLine(line string) (Progress, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Progress{}, false
	}
	if kind, rest, ok := strings.Cut(line, ":"); ok && (kind == "dlstatus" || kind == "pmstatus") {
		// KIND:PACKAGE:PERCENT:DESCRIPTION, the description free text.
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) != 3 {
			return Progress{}, false
		}
		var progress Progress
		if percent, err := strconv.ParseFloat(parts[1], 64); err == nil {
			progress.Percent = clampPercent(int(percent))
		}
		progress.Detail = BoundProgressDetail(lowerFirst(parts[2]))
		return progress, progress.Detail != "" || progress.Percent > 0
	}
	if aptStatus(line) {
		// Another of apt's lines (pmconffile, pmerror, media-change): not a
		// detail to show.
		return Progress{}, false
	}
	var progress Progress
	words := line
	for {
		word, rest, _ := strings.Cut(words, " ")
		key, value, ok := strings.Cut(word, "=")
		if !ok {
			break
		}
		switch key {
		case "bytes":
			doneText, totalText, hasTotal := strings.Cut(value, "/")
			done, err := strconv.ParseInt(doneText, 10, 64)
			var total int64
			if err == nil && hasTotal {
				total, err = strconv.ParseInt(totalText, 10, 64)
			}
			if err != nil || done < 0 || total < 0 {
				ok = false
				break
			}
			progress.Done, progress.Total = done, total
		case "percent":
			percent, err := strconv.Atoi(value)
			if err != nil {
				ok = false
				break
			}
			progress.Percent = clampPercent(percent)
		default:
			ok = false
		}
		if !ok {
			// Not a word of ours: the detail starts here.
			progress.Detail = BoundProgressDetail(words)
			return progress, true
		}
		words = strings.TrimLeft(rest, " ")
		if words == "" {
			break
		}
	}
	progress.Detail = BoundProgressDetail(words)
	return progress, true
}

// aptStatus reports whether line is one of apt's status lines.
func aptStatus(line string) bool {
	kind, _, _ := strings.Cut(line, ":")
	switch kind {
	case "pmconffile", "pmerror", "media-change", "dlstatus", "pmstatus":
		return true
	}
	return false
}

func clampPercent(percent int) int {
	return min(max(percent, 0), 100)
}

func lowerFirst(s string) string {
	s = strings.TrimSpace(s)
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// BoundProgressDetail folds a detail onto one line of valid UTF-8, at most
// MaxProgressDetail bytes. It is still the box's text: a reader escapes it.
func BoundProgressDetail(detail string) string {
	detail = strings.Join(strings.Fields(strings.ToValidUTF8(detail, "\ufffd")), " ")
	if len(detail) <= MaxProgressDetail {
		return detail
	}
	cut := MaxProgressDetail
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut]
}

// Off reports whether the box reported feature off. A feature it does not
// report at all is not off: it is content the box came with, as everything
// was before features were provisioned.
func (s Status) Off(feature string) bool {
	return s.Features[feature].State == StateOff
}

// ReadStatus reads the document at path, reporting false when there is none.
func ReadStatus(path string) (Status, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return Status{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	return status, true, nil
}

// FeatureOff reports whether the box's provision.json at path reports
// feature off. A box with no document, or one that cannot be read, has
// nothing off: a runtime integration then wires as it always has.
func FeatureOff(path, feature string) bool {
	status, ok, err := ReadStatus(path)
	return err == nil && ok && status.Off(feature)
}
