// Package hostid resolves this machine's stable Discobox client identity.
//
// The identity is shared, not per-process: a CLI and a control plane running as
// the same user on the same machine must resolve the same value, because that
// is how the server recognizes a create request as coming from its own
// filesystem and binds the source instead of asking for a push. That agreement
// is why this lives here rather than in either component.
//
// See docs/adr/0001-sandbox-origin-and-remote-source-push.md.
package hostid

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"

	"github.com/obot-platform/discobox/id"
)

const (
	appName = "discobox"

	// fileName is the file under the XDG config directory holding the
	// generated identity.
	fileName = "host-id"

	// EnvVar overrides the persisted identity. Set it where the config
	// directory is ephemeral (CI, containers) and a fresh identity per run
	// would otherwise accumulate unrelated sandboxes in listings.
	EnvVar = "DISCOBOX_HOST_ID"
)

// Get returns this machine's identity, generating and persisting one on first
// use.
//
// The ID is opaque and generated, never derived from the machine. Every
// available machine signal is unfit: hostnames are neither unique nor stable,
// MAC addresses are randomized, and /etc/machine-id is absent on macOS and
// baked into container images, so every container from one image would report
// the same identity. The requirement is only that the value is unique and
// stable, not that it describes any hardware.
func Get() (string, error) {
	if env := strings.TrimSpace(os.Getenv(EnvVar)); env != "" {
		return env, nil
	}
	path, err := Path()
	if err != nil {
		return "", err
	}
	if existing, ok := read(path); ok {
		return existing, nil
	}
	generated, err := id.New(id.PrefixHost)
	if err != nil {
		return "", fmt.Errorf("generate host ID: %w", err)
	}
	return write(path, generated)
}

// Path returns the file holding the persisted identity.
func Path() (string, error) {
	if strings.TrimSpace(xdg.ConfigHome) == "" {
		return "", errors.New("resolve config directory: XDG config home is empty")
	}
	return filepath.Join(xdg.ConfigHome, appName, fileName), nil
}

// read reports the stored identity. A file that does not hold a well-formed ID
// is treated as absent so a truncated or hand-edited file regenerates instead
// of wedging every command.
func read(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(data))
	if !strings.HasPrefix(value, id.PrefixHost+"_") || !id.IsGenerated(value) {
		return "", false
	}
	return value, true
}

// write installs value as this machine's identity and returns the identity that
// ends up stored, which is not value when another process got there first.
//
// The file is written fully before being linked into place, so a reader never
// observes a torn value. Linking rather than renaming is what makes the first
// writer win: rename would clobber, letting concurrent first runs each install
// their own ID and split one machine's sandboxes across several identities.
func write(path, value string) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, fileName+".*")
	if err != nil {
		return "", fmt.Errorf("create host ID file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod host ID file: %w", err)
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write host ID file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write host ID file: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("install host ID file %s: %w", path, err)
		}
		// Someone holds the path. If they left a usable ID, they won the race
		// and it becomes this machine's identity too.
		if stored, ok := read(path); ok {
			return stored, nil
		}
		// Otherwise the file is corrupt and there is no identity to preserve,
		// so replacing it is safe and is the only way to recover.
		if err := os.Rename(tmp.Name(), path); err != nil {
			return "", fmt.Errorf("replace unusable host ID file %s: %w", path, err)
		}
	}
	return value, nil
}
