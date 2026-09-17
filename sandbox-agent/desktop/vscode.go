package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tailscale/hujson"
)

// VS Code is the one program on this desktop that scales itself twice and has
// nowhere else to be told otherwise.
//
// Chromium derives its device scale factor by multiplying Xft/DPI (192 at 2x,
// so 2) by GDK_SCALE (2), and comes out at scale squared. The image's own
// Chromium is corrected by /etc/chromium.d (see image/desktop/chromium.d), but
// an Electron app has no such launcher hook, and measured on this desktop VS
// Code's devicePixelRatio is 4 at 2x — and 2, the right answer, only once
// GDK_SCALE is gone, which the rest of the desktop needs.
//
// VS Code reads a fixed set of Chromium switches from ~/.vscode/argv.json at
// launch, force-device-scale-factor among them. Naming the scale there stops
// the inference, exactly as the Chromium hook does, so it is written wherever
// scale.env is: it is launch-time scale, read once when VS Code starts.
const (
	// DefaultVSCodeArgv is VS Code's own argument file.
	DefaultVSCodeArgv = "~/.vscode/argv.json"

	vscodeScaleKey = "force-device-scale-factor"
)

// writeVSCodeScale sets force-device-scale-factor in VS Code's argv.json.
//
// The file is VS Code's and the user's: it is JSON with comments, VS Code seeds
// it with a crash-reporter id on first launch, and a person may have added
// switches of their own. So it is patched rather than written — the one key
// added or replaced, every comment and every other key kept, the whitespace
// normalized so the new key is on a line of its own — and
// created only when VS Code has not created it yet. A file that does not parse
// is left alone and reported: rewriting what someone half-edited would lose it.
func writeVSCodeScale(path string, scale int) error {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = []byte("// Launch arguments for VS Code. See https://code.visualstudio.com/docs/configure/command-line\n{\n}\n")
	case err != nil:
		return fmt.Errorf("read %s: %w", path, err)
	}
	value, err := hujson.Parse(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	op := "add"
	if value.Find("/"+vscodeScaleKey) != nil {
		op = "replace"
	}
	patch := fmt.Sprintf(`[{"op":%q,"path":"/%s","value":%s}]`, op, vscodeScaleKey, strconv.Itoa(scale))
	if err := value.Patch([]byte(patch)); err != nil {
		return fmt.Errorf("set %s in %s: %w", vscodeScaleKey, path, err)
	}
	value.Format()
	updated := value.Pack()
	if bytes.Equal(updated, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Moved into place whole, for the reason scale.env is: VS Code starting
	// while this is half-written would read a truncated file.
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, updated, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
