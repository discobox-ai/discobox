package services

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxservices"
)

// writeService puts one declaration in root's service directory.
func writeService(t *testing.T, root, name, body string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const apiScript = `#!/bin/bash
#---
# name: Discobox API
# description: Runs the server with hot reload
#---
exec task dev:server
`

func TestDiscoverReadsDeclarations(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-discobox-api.sh", apiScript, 0o755)
	writeService(t, root, "15-otel.sh", "#!/bin/bash\n#---\n# name: OTEL\n#---\nexec dashboard\n", 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("discovered %d services, want 2: %+v", len(defs), defs)
	}
	// Filename order, which is what the numeric prefix is for.
	if defs[0].ID != "discobox-api" || defs[1].ID != "otel" {
		t.Fatalf("ids = %q, %q; want discobox-api, otel", defs[0].ID, defs[1].ID)
	}
	if defs[0].Name != "Discobox API" {
		t.Errorf("name = %q, want %q", defs[0].Name, "Discobox API")
	}
	if defs[0].Description != "Runs the server with hot reload" {
		t.Errorf("description = %q", defs[0].Description)
	}
	if defs[0].FileName != "10-discobox-api.sh" {
		t.Errorf("fileName = %q", defs[0].FileName)
	}
	if !defs[0].Runnable() {
		t.Errorf("problem = %q, want none", defs[0].Problem)
	}
	if defs[0].Path != filepath.Join(root, DirName, "10-discobox-api.sh") {
		t.Errorf("path = %q", defs[0].Path)
	}
}

// A repository that declares nothing is the common case and must cost nothing
// but a failed read.
func TestDiscoverAbsentDirectory(t *testing.T) {
	defs, err := Discover("", t.TempDir())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("discovered %d services, want none", len(defs))
	}
}

// A name defaults from the filename, so a declaration that says nothing is
// still addressable and still readable.
func TestDiscoverDefaultsNameFromFilename(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "20-web-ui.sh", "#!/bin/sh\n#---\n#---\nexec serve\n", 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "Web Ui" {
		t.Fatalf("defs = %+v, want one named %q", defs, "Web Ui")
	}
}

// A declaration that cannot run is listed with the reason rather than dropped:
// a file the author believes is a service and that nothing ever mentions is the
// failure this avoids.
func TestDiscoverReportsUnrunnableDeclarations(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-not-executable.sh", apiScript, 0o644)
	writeService(t, root, "20-no-shebang.sh", "#---\n# name: Nope\n#---\necho hi\n", 0o755)
	writeService(t, root, "30-no-front-matter.sh", "#!/bin/sh\necho hi\n", 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("discovered %d services, want 3", len(defs))
	}
	// Windows has no executable bit to withhold, so the first fixture is a
	// runnable script there and only the other two are declarations to reject.
	unrunnable := defs
	if runtime.GOOS == "windows" {
		unrunnable = defs[1:]
	}
	for _, def := range unrunnable {
		if def.Runnable() {
			t.Errorf("%s: expected a problem, got none", def.ID)
		}
	}
	if runtime.GOOS != "windows" {
		if got := defs[0].Problem; got != "script is not executable" {
			t.Errorf("problem = %q, want %q", got, "script is not executable")
		}
	}
	if got := defs[1].Problem; got != "script must start with a shebang line" {
		t.Errorf("problem = %q, want %q", got, "script must start with a shebang line")
	}
	// A file with no front matter at all is a declaration error, not a service
	// with no name: `.discobox/services` is not a scripts folder.
	if defs[2].Problem == "" {
		t.Error("a file with no front matter must report a problem")
	}
}

// Two files whose names normalize to one id would otherwise take turns being
// "the" service depending on directory order.
func TestDiscoverReportsDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	writeService(t, root, "20-api.sh", apiScript, 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("discovered %d services, want 2", len(defs))
	}
	if !defs[0].Runnable() {
		t.Errorf("the first declaration keeps the id: %q", defs[0].Problem)
	}
	if defs[1].Runnable() {
		t.Error("the second declaration of an id must report the conflict")
	}
}

func TestDiscoverSkipsDirectoriesAndDotfiles(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	writeService(t, root, ".hidden.sh", apiScript, 0o755)
	if err := os.MkdirAll(filepath.Join(root, DirName, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 1 || defs[0].ID != "api" {
		t.Fatalf("defs = %+v, want just api", defs)
	}
}

func TestDiscoverReadsDeclaredPorts(t *testing.T) {
	root := t.TempDir()
	// Every spelling of the field means the same thing, because a file is
	// written by a person rather than by the schema.
	writeService(t, root, "10-list.sh", "#!/bin/bash\n#---\n# ports: [8080, 5432]\n#---\nexec up\n", 0o755)
	writeService(t, root, "20-scalar.sh", "#!/bin/bash\n#---\n# ports: 9000, 9001 9002\n#---\nexec up\n", 0o755)
	writeService(t, root, "30-singular.sh", "#!/bin/bash\n#---\n# port: 3000\n#---\nexec up\n", 0o755)
	writeService(t, root, "40-none.sh", apiScript, 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	want := [][]int{{8080, 5432}, {9000, 9001, 9002}, {3000}, nil}
	for i, def := range defs {
		if !equalInts(def.Ports, want[i]) {
			t.Errorf("%s ports = %v, want %v", def.FileName, def.Ports, want[i])
		}
		if !def.Runnable() {
			t.Errorf("%s problem = %q, want none", def.FileName, def.Problem)
		}
	}
}

func TestDiscoverRejectsAPortThatIsNotOne(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-bad.sh", "#!/bin/bash\n#---\n# ports: 8080, http\n#---\nexec up\n", 0o755)
	writeService(t, root, "20-range.sh", "#!/bin/bash\n#---\n# ports: 70000\n#---\nexec up\n", 0o755)

	defs, err := Discover("", root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	for _, def := range defs {
		// A declaration whose ports cannot be read is listed with the reason,
		// not run without them: the author meant something by that value.
		if def.Runnable() {
			t.Errorf("%s is runnable, want a reported problem", def.FileName)
		}
		if len(def.Ports) != 0 {
			t.Errorf("%s ports = %v, want none kept from a rejected list", def.FileName, def.Ports)
		}
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// writeBuiltin writes a declaration into an image services directory.
func writeBuiltin(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// The desktop's shape: ports and a protocol, nothing to run. It must not be
// reported as a broken service — a declaration that starts nothing has no
// shebang and no executable bit, and both are Problems only for a script the
// sandbox is meant to launch.
func TestADeclarationThatStartsNothingIsNotBroken(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-desktop.sh",
		"#---\n# name: Desktop\n# port: 6900\n# protocol: http\n# start: never\n#---\n", 0o644)

	defs, err := Discover(builtin, t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected one declaration, got %+v", defs)
	}
	def := defs[0]
	if def.Problem != "" {
		t.Fatalf("declaration reported a problem: %q", def.Problem)
	}
	if def.Runnable() {
		t.Fatalf("a declaration that starts nothing must not be runnable")
	}
	if def.Start != StartNever {
		t.Fatalf("start = %q, want %q", def.Start, StartNever)
	}
	if def.Protocol != "http" || len(def.Ports) != 1 || def.Ports[0] != 6900 {
		t.Fatalf("unexpected declaration: %+v", def)
	}
	if !def.Builtin {
		t.Fatalf("a declaration from the image is not marked builtin: %+v", def)
	}
}

// An ordinary script is still validated as one, so `start: never` cannot be
// inferred from a missing executable bit.
func TestAScriptThatStartsNothingIsStillCheckedWhenItSaysNothing(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-thing.sh", "#!/bin/bash\n#---\n# port: 8080\n#---\nexec up\n", 0o644)

	defs, err := Discover(builtin, t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 1 || defs[0].Problem == "" {
		t.Fatalf("a non-executable script should still be a problem: %+v", defs)
	}
}

// A protocol the port watcher cannot act on is an error, not a field quietly
// dropped: the difference between them is whether a socket-activated service
// gets started by a classification probe.
func TestAnUnknownProtocolIsAProblem(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-thing.sh",
		"#---\n# port: 6900\n# protocol: gopher\n# start: never\n#---\n", 0o644)

	defs, err := Discover(builtin, t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 1 || !strings.Contains(defs[0].Problem, "protocol") {
		t.Fatalf("expected a protocol problem, got %+v", defs)
	}
}

// Both directories are read, the image's first — which is what settles a port
// two declarations name, since the port watcher takes the first it is given.
// A repository declaration wins on a shared id, the way its skills do.
func TestBothDirectoriesAreReadAndTheRepositoryWinsOnAnID(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-desktop.sh",
		"#---\n# port: 6900\n# protocol: http\n# start: never\n#---\n", 0o644)
	writeBuiltin(t, builtin, "20-shared.sh",
		"#---\n# port: 7000\n# protocol: tcp\n# start: never\n#---\n", 0o644)

	root := t.TempDir()
	writeService(t, root, "20-shared.sh", "#!/bin/bash\n#---\n# port: 9999\n#---\nexec up\n", 0o755)

	defs, err := Discover(builtin, root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byID := map[string]Definition{}
	var order []string
	for _, def := range defs {
		byID[def.ID] = def
		order = append(order, def.ID)
	}
	if len(defs) != 2 {
		t.Fatalf("expected the desktop and one shared id, got %+v", order)
	}
	if order[0] != "desktop" {
		t.Fatalf("image declarations must come first, got %v", order)
	}
	shared := byID["shared"]
	if shared.Builtin || len(shared.Ports) != 1 || shared.Ports[0] != 9999 {
		t.Fatalf("the repository declaration did not win on the shared id: %+v", shared)
	}
}

// A stated id replaces the filename-derived one and survives a rename, which is
// the point: a client matches on it.
func TestAStatedIDReplacesTheFilenameDerivedOne(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-desktop.sh",
		"#---\n# id: "+sandboxservices.DesktopID+"\n# name: Desktop\n# port: 6900\n# protocol: http\n# start: never\n#---\n", 0o644)

	defs, err := Discover(builtin, t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected one declaration, got %+v", defs)
	}
	if defs[0].Problem != "" {
		t.Fatalf("declaration reported a problem: %q", defs[0].Problem)
	}
	// Not `ai-discobox-desktop`: NormalizeID would turn the dots into dashes,
	// and every match on the id would quietly miss.
	if defs[0].ID != sandboxservices.DesktopID {
		t.Fatalf("id = %q, want the stated %q", defs[0].ID, sandboxservices.DesktopID)
	}
	if defs[0].Name != "Desktop" {
		t.Fatalf("name = %q, want the stated display name", defs[0].Name)
	}
}

// An id is a name other things match on, so a value that would match
// inconsistently is refused rather than normalized into something else.
func TestAnIDThatIsNotReverseDNSIsAProblem(t *testing.T) {
	for _, id := range []string{"Ai.Discobox.Desktop", "ai..desktop", "ai.discobox.", ".desktop", "ai discobox", "9lives.thing"} {
		t.Run(id, func(t *testing.T) {
			builtin := t.TempDir()
			writeBuiltin(t, builtin, "10-thing.sh",
				"#---\n# id: "+id+"\n# port: 6900\n# start: never\n#---\n", 0o644)
			defs, err := Discover(builtin, t.TempDir())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(defs) != 1 || !strings.Contains(defs[0].Problem, "id:") {
				t.Fatalf("expected an id problem for %q, got %+v", id, defs)
			}
		})
	}
}

// The reserved namespace is the image's. A repository declaring the desktop's
// id would otherwise replace it and take its link with it — repository
// declarations win on a shared id — which is a confusing accident rather than
// anything anybody wants.
//
// The builtin directory ships the real thing here, because the takeover is what
// this is about: asserting only that the repository's file gets a Problem
// passes even when the image's declaration has already been evicted by it.
func TestTheDiscoboxNamespaceIsReservedForTheImage(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-desktop.sh",
		"#---\n# id: "+sandboxservices.DesktopID+"\n# name: Desktop\n# port: 6900\n# protocol: http\n# start: never\n#---\n", 0o644)

	root := t.TempDir()
	writeService(t, root, "10-mine.sh",
		"#!/bin/bash\n#---\n# id: "+sandboxservices.DesktopID+"\n# port: 7100\n#---\nexec up\n", 0o755)

	defs, err := Discover(builtin, root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected both declarations to be listed, got %+v", defs)
	}

	// The image's declaration survives, with its own port.
	var desktop *Definition
	for i := range defs {
		if defs[i].ID == sandboxservices.DesktopID {
			desktop = &defs[i]
		}
	}
	if desktop == nil {
		t.Fatalf("the image's desktop declaration was evicted by a repository claiming its id: %+v", defs)
	}
	if !desktop.Builtin {
		t.Fatalf("%q is the repository's declaration, not the image's: %+v", sandboxservices.DesktopID, *desktop)
	}
	if len(desktop.Ports) != 1 || desktop.Ports[0] != 6900 {
		t.Fatalf("the desktop's ports = %v, want the image's 6900", desktop.Ports)
	}

	// The repository's is listed with the reason, not dropped: a declaration
	// that silently vanishes is indistinguishable from one nobody wrote. It
	// keeps its filename-derived id, so nothing it says lands on the reserved
	// one.
	var mine *Definition
	for i := range defs {
		if defs[i].FileName == "10-mine.sh" {
			mine = &defs[i]
		}
	}
	if mine == nil {
		t.Fatalf("the repository's declaration was dropped: %+v", defs)
	}
	if !strings.Contains(mine.Problem, "reserved") {
		t.Fatalf("a repository claimed %q without a problem: %+v", sandboxservices.DesktopID, *mine)
	}
	if mine.ID == sandboxservices.DesktopID {
		t.Fatalf("a refused declaration kept the reserved id, so its port ships as the desktop's: %+v", *mine)
	}
	if mine.Runnable() {
		t.Fatalf("a reserved-id declaration must not be runnable")
	}
}

// The image may use it, which is the whole point of reserving it.
func TestTheImageMayUseTheReservedNamespace(t *testing.T) {
	builtin := t.TempDir()
	writeBuiltin(t, builtin, "10-desktop.sh",
		"#---\n# id: "+sandboxservices.DesktopID+"\n# port: 6900\n# start: never\n#---\n", 0o644)

	defs, err := Discover(builtin, t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(defs) != 1 || defs[0].Problem != "" {
		t.Fatalf("the image cannot use its own namespace: %+v", defs)
	}
}

// The declaration the image actually ships, parsed from the tree it is built
// from. Everything else here tests the rules; this tests the one file that has
// to obey them, because a typo in it is a desktop that never appears — or
// worse, one probed into starting.
func TestTheShippedDesktopDeclarationIsValid(t *testing.T) {
	defs, err := Discover(filepath.Join("..", "image", "services"), t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var desktop *Definition
	for i := range defs {
		if defs[i].ID == sandboxservices.DesktopID {
			desktop = &defs[i]
		}
	}
	if desktop == nil {
		t.Fatalf("the image ships no %s declaration; got %+v", sandboxservices.DesktopID, defs)
	}
	if desktop.Problem != "" {
		t.Fatalf("the shipped declaration has a problem: %q", desktop.Problem)
	}
	if desktop.Start != StartNever {
		t.Fatalf("start = %q; systemd starts the desktop, not the sandbox", desktop.Start)
	}
	// The two fields the whole arrangement rests on: without the port it is
	// invisible to discovery, and without the protocol it gets probed — and
	// probing a socket-activated port is what starts it.
	if len(desktop.Ports) != 1 || desktop.Ports[0] != 6900 {
		t.Fatalf("ports = %v, want [6900]", desktop.Ports)
	}
	if desktop.Protocol != "http" {
		t.Fatalf("protocol = %q, want http; an unstated protocol would be probed", desktop.Protocol)
	}
	if desktop.Name == "" {
		t.Fatalf("the declaration has no display name for a client to label the link with")
	}
}
