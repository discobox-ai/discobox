package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

// What VS Code writes on its first launch: comments, a commented-out switch,
// and a crash-reporter id that must survive.
const vscodeSeededArgv = `// This configuration file allows you to pass permanent command line arguments to VS Code.
{
	// Use software rendering instead of hardware accelerated rendering.
	// "disable-hardware-acceleration": true,

	// Allows to disable crash reporting.
	"enable-crash-reporter": true,

	// Do not edit this value.
	"crash-reporter-id": "30ba8710-8f10-4693-842b-4b3c10e399ca"
}
`

func readVSCodeScale(t *testing.T, path string) (float64, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := hujson.Parse(data)
	if err != nil {
		t.Fatalf("argv.json no longer parses: %v\n%s", err, data)
	}
	found := value.Find("/" + vscodeScaleKey)
	if found == nil {
		t.Fatalf("no %s in\n%s", vscodeScaleKey, data)
	}
	literal, _ := found.Value.(hujson.Literal)
	return literal.Float(), string(data)
}

func TestWriteVSCodeScaleKeepsWhatVSCodeWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".vscode", "argv.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(vscodeSeededArgv), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeVSCodeScale(path, 2); err != nil {
		t.Fatal(err)
	}
	scale, text := readVSCodeScale(t, path)
	if scale != 2 {
		t.Fatalf("scale = %v, want 2", scale)
	}
	for _, kept := range []string{`"crash-reporter-id"`, `"30ba8710-8f10-4693-842b-4b3c10e399ca"`, `// "disable-hardware-acceleration": true,`, "// Do not edit this value."} {
		if !strings.Contains(text, kept) {
			t.Errorf("lost %q:\n%s", kept, text)
		}
	}

	// A scale change replaces the value rather than adding a second key.
	if err := writeVSCodeScale(path, 1); err != nil {
		t.Fatal(err)
	}
	scale, text = readVSCodeScale(t, path)
	if scale != 1 || strings.Count(text, vscodeScaleKey) != 1 {
		t.Fatalf("scale = %v with %d keys:\n%s", scale, strings.Count(text, vscodeScaleKey), text)
	}
}

func TestWriteVSCodeScaleCreatesTheFileBeforeVSCodeHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".vscode", "argv.json")
	if err := writeVSCodeScale(path, 2); err != nil {
		t.Fatal(err)
	}
	if scale, _ := readVSCodeScale(t, path); scale != 2 {
		t.Fatalf("scale = %v, want 2", scale)
	}
}

func TestWriteVSCodeScaleLeavesAFileItCannotParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argv.json")
	broken := "{ \"half\": \n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeVSCodeScale(path, 2); err == nil {
		t.Fatal("a file that does not parse was not reported")
	}
	if data, _ := os.ReadFile(path); string(data) != broken {
		t.Fatalf("the file was rewritten: %q", data)
	}
}

// The display writes it wherever it writes scale.env, and never fails the
// scale change over it.
func TestAdoptScaleCarriesTheScaleToVSCode(t *testing.T) {
	dir := t.TempDir()
	d := NewDisplay(":99")
	d.EnvDir = filepath.Join(dir, "env")
	d.VSCodeArgv = filepath.Join(dir, ".vscode", "argv.json")
	if err := d.AdoptScale(2); err != nil {
		t.Fatal(err)
	}
	if scale, _ := readVSCodeScale(t, d.VSCodeArgv); scale != 2 {
		t.Fatalf("scale = %v, want 2", scale)
	}

	if err := os.WriteFile(d.VSCodeArgv, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.AdoptScale(1); err != nil {
		t.Fatalf("an unreadable argv.json failed the scale change: %v", err)
	}
}
