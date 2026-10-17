package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/fsdurable"
	"github.com/discobox-ai/discobox/provisionspec"
)

// RecordsDir is where a box keeps what has been applied to it. It is on the
// root filesystem, never a declared volume, so a container recreated from an
// image or a snapshot has the records of exactly what that filesystem holds
// (ADR 26-10-16-423 §3).
const RecordsDir = "/opt/discobox/provision"

// Records is a box's store of applied steps, and of the sandbox its sandbox
// phase last ran for. Steps live under steps/<feature>/<step>.json.
type Records struct {
	Dir string
}

// StepRecord is what is kept for one applied step.
type StepRecord struct {
	Digest    string    `json:"digest"`
	AppliedAt time.Time `json:"appliedAt"`
}

func (r Records) stepPath(feature, step string) string {
	return filepath.Join(r.Dir, "steps", feature, step+".json")
}

// Step is the record of a feature's step, and whether there is one. A record
// that cannot be decoded is no record: the step is applied again.
func (r Records) Step(feature, step string) (StepRecord, bool, error) {
	data, err := os.ReadFile(r.stepPath(feature, step))
	if errors.Is(err, fs.ErrNotExist) {
		return StepRecord{}, false, nil
	}
	if err != nil {
		return StepRecord{}, false, err
	}
	record, ok := decodeStepRecord(data)
	return record, ok, nil
}

// decodeStepRecord reads a record, reporting false for one torn or empty.
func decodeStepRecord(data []byte) (StepRecord, bool) {
	var record StepRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return StepRecord{}, false
	}
	return record, record.Digest != ""
}

// RecordStep records a step as applied with digest.
func (r Records) RecordStep(feature, step string, record StepRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.stepPath(feature, step), append(data, '\n'), 0o644)
}

// Forget removes a step's record, so the step is applied again if it
// returns.
func (r Records) Forget(feature, step string) error {
	if err := os.Remove(r.stepPath(feature, step)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (r Records) sandboxPath() string {
	return filepath.Join(r.Dir, "sandbox")
}

// Sandbox is the sandbox the box's sandbox phase last ran for, or "" when it
// has not run here.
func (r Records) Sandbox() string {
	data, err := os.ReadFile(r.sandboxPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// RecordSandbox records that the sandbox phase ran for sandboxID.
func (r Records) RecordSandbox(sandboxID string) error {
	return writeFileAtomic(r.sandboxPath(), []byte(sandboxID+"\n"), 0o600)
}

// StepDigest keys a step: a digest of the feature it belongs to, its
// definition, and the content of every file it names, so editing a script
// re-applies the step that runs it and no other.
func StepDigest(feature provisionspec.Feature, step provisionspec.Step) (string, error) {
	definition, err := json.Marshal(struct {
		APIVersion string             `json:"apiVersion"`
		Feature    string             `json:"feature"`
		Step       provisionspec.Step `json:"step"`
	}{provisionspec.APIVersion, feature.Name, step})
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	writeField(hash, definition)
	for _, file := range step.Files() {
		data, err := os.ReadFile(filepath.Join(feature.Dir, filepath.FromSlash(file)))
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", file, err)
		}
		writeField(hash, []byte(file))
		writeField(hash, data)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// writeField writes a length-prefixed field, so no two sequences of fields
// hash alike.
func writeField(w interface{ Write([]byte) (int, error) }, data []byte) {
	_, _ = fmt.Fprintf(w, "%d:", len(data))
	_, _ = w.Write(data)
}

// writeFileAtomic replaces path with data, so a reader never sees it torn and
// a crash leaves the old content or the new.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return fsdurable.SyncDir(dir)
}
