package provisionspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
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

// Done reports whether nothing more will happen to what is in s this boot.
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
