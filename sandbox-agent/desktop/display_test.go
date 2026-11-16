package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeXrandr is enough of xrandr to size a screen with: it keeps the modes it
// was given in a file, refuses to switch to one it does not have, and reports
// the current size. Removing the file is an X server restarting. The file is
// written into the script: xrandr is given an environment of the display's.
const fakeXrandr = `#!/bin/sh
modes=%s
case "$1" in
--query)
  current=$(cat "$modes.current" 2>/dev/null || echo "1280 x 1024")
  echo "Screen 0: minimum 64 x 64, current $current, maximum 32767 x 32767"
  echo "DUMMY0 connected $current+0+0 0mm x 0mm"
  cat "$modes" 2>/dev/null || true
  ;;
--newmode) echo "   $2" >> "$modes" ;;
--addmode) ;;
--output)
  grep -q " $4$" "$modes" 2>/dev/null || { echo "xrandr: cannot find mode $4" >&2; exit 1; }
  echo "$4" | sed 's/_.*//; s/x/ x /' > "$modes.current"
  ;;
esac
`

// An X server that restarts comes back without any of the modes this process
// made on the last one. A resize to a size it made before must still work:
// the remembered modes are the old server's, and are made again.
func TestAResizeAfterTheXServerRestartedMakesItsModeAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the desktop runs on Linux")
	}
	bin := t.TempDir()
	modes := filepath.Join(t.TempDir(), "modes")
	writeScript(t, filepath.Join(bin, "xrandr"), fmt.Sprintf(fakeXrandr, modes))
	writeScript(t, filepath.Join(bin, "gtf"), "#!/bin/sh\necho '  Modeline \"x\" 1 2 3 4 5 6 7 8 9'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	display := NewDisplay(":91")
	if _, err := display.Resize(t.Context(), 1000, 700); err != nil {
		t.Fatalf("first resize: %v", err)
	}
	// The X server restarts: its modes are gone, and it is at its default.
	if err := os.Remove(modes); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(modes + ".current"); err != nil {
		t.Fatal(err)
	}
	geometry, err := display.Resize(t.Context(), 1000, 700)
	if err != nil {
		t.Fatalf("resize after the X server restarted: %v", err)
	}
	current, _ := os.ReadFile(modes + ".current")
	if got := strings.TrimSpace(string(current)); got != "960 x 640" {
		t.Fatalf("the screen is %q, want the 960 x 640 asked for (geometry %+v)", got, geometry)
	}
}
