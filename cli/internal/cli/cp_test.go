package cli

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestSplitSCPArgsFindsTheOperands: the operands are rewritten, so a value that
// happens to look like a path — `-o ProxyJump=x`, `-l 1024` — must not be
// mistaken for one.
func TestSplitSCPArgsFindsTheOperands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		options []string
		paths   []string
	}{
		{
			name:  "plain copy",
			args:  []string{"a.txt", "mybox:/tmp/a.txt"},
			paths: []string{"a.txt", "mybox:/tmp/a.txt"},
		},
		{
			name:    "leading flags",
			args:    []string{"-r", "-p", "mybox:/dist", "./dist"},
			options: []string{"-r", "-p"},
			paths:   []string{"mybox:/dist", "./dist"},
		},
		{
			name:    "detached value",
			args:    []string{"-l", "1024", "a.txt", "mybox:"},
			options: []string{"-l", "1024"},
			paths:   []string{"a.txt", "mybox:"},
		},
		{
			name:    "attached value",
			args:    []string{"-l1024", "a.txt", "mybox:"},
			options: []string{"-l1024"},
			paths:   []string{"a.txt", "mybox:"},
		},
		{
			name:    "bundle ending in a value option",
			args:    []string{"-rl", "1024", "a.txt", "mybox:"},
			options: []string{"-rl", "1024"},
			paths:   []string{"a.txt", "mybox:"},
		},
		{
			// The `r` belongs to the -o value, not to scp.
			name:    "value letters are not flags",
			args:    []string{"-o", "Compression=yes", "a.txt", "mybox:"},
			options: []string{"-o", "Compression=yes"},
			paths:   []string{"a.txt", "mybox:"},
		},
		{
			// Options after the operands are still options: scp's usage puts
			// them first, and only glibc's permuting getopt forgives otherwise.
			name:    "trailing flag",
			args:    []string{"a.txt", "mybox:", "-v"},
			options: []string{"-v"},
			paths:   []string{"a.txt", "mybox:"},
		},
		{
			name:    "separator ends the options",
			args:    []string{"-r", "--", "-weird.txt", "mybox:"},
			options: []string{"-r"},
			paths:   []string{"-weird.txt", "mybox:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, paths := splitSCPArgs(tc.args)
			if strings.Join(options, " ") != strings.Join(tc.options, " ") {
				t.Errorf("options = %v, want %v", options, tc.options)
			}
			if strings.Join(paths, " ") != strings.Join(tc.paths, " ") {
				t.Errorf("paths = %v, want %v", paths, tc.paths)
			}
		})
	}
}

// TestSplitCPPathFollowsSCPsColonRule: whether an operand is remote has to be
// decided exactly as scp decides it, or a path rewritten here means something
// else once scp reads it back.
func TestSplitCPPathFollowsSCPsColonRule(t *testing.T) {
	for _, tc := range []struct {
		operand   string
		reference string
		path      string
		remote    bool
	}{
		{operand: "mybox:/tmp/a.txt", reference: "mybox", path: "/tmp/a.txt", remote: true},
		{operand: "sbx_devbox00000001:notes.md", reference: "sbx_devbox00000001", path: "notes.md", remote: true},
		// A bare colon is this directory's discobox, the one place this differs
		// from scp — where `:x` is just the local file `./x`.
		{operand: ":notes.md", reference: "", path: "notes.md", remote: true},
		// A remote home directory.
		{operand: "mybox:", reference: "mybox", path: "", remote: true},
		{operand: "a.txt", remote: false},
		{operand: "./dist", remote: false},
		{operand: "/tmp/a.txt", remote: false},
		// The slash comes first, so the colon is part of a filename.
		{operand: "./weird:name", remote: false},
		{operand: "/tmp/weird:name", remote: false},
	} {
		t.Run(tc.operand, func(t *testing.T) {
			reference, path, remote := splitCPPath(tc.operand)
			if remote != tc.remote {
				t.Fatalf("remote = %v, want %v", remote, tc.remote)
			}
			if remote && (reference != tc.reference || path != tc.path) {
				t.Fatalf("split = (%q, %q), want (%q, %q)", reference, path, tc.reference, tc.path)
			}
		})
	}
}

// TestLocalSCPPathSurvivesSCPsColonRule: an operand this command reads as local
// must still be local once scp applies the same rule to it, which for a
// relative path with a colon in it means putting a slash in front.
func TestLocalSCPPathSurvivesSCPsColonRule(t *testing.T) {
	for _, tc := range []struct{ operand, want string }{
		{operand: "a.txt", want: "a.txt"},
		{operand: "./dist", want: "./dist"},
		{operand: "/tmp/weird:name", want: "/tmp/weird:name"},
		{operand: "./weird:name", want: "./weird:name"},
		// Only reachable through splitCPPath's slash-first rule, and the one
		// case scp would otherwise read as a host called "sub".
		{operand: "sub/dir:name", want: "./sub/dir:name"},
	} {
		t.Run(tc.operand, func(t *testing.T) {
			if got := localSCPPath(tc.operand); got != tc.want {
				t.Fatalf("localSCPPath(%q) = %q, want %q", tc.operand, got, tc.want)
			}
		})
	}
}

// TestWindowsDrivePathIsOnlyAPathOnWindows: `C:/src` names a directory on
// Windows and a host called C everywhere else.
func TestWindowsDrivePathIsOnlyAPathOnWindows(t *testing.T) {
	_, _, remote := splitCPPath(`C:\src\app`)
	if want := runtime.GOOS != "windows"; remote != want {
		t.Fatalf(`splitCPPath("C:\\src\\app") remote = %v on %s, want %v`, remote, runtime.GOOS, want)
	}
}

// TestSCPArgsPlacesEverythingWhereSCPReadsIt: `scp [options] source ... target`
// is positional, and this command supplies options of its own, so the assembled
// list is what decides whether a path is read as a path.
func TestSCPArgsPlacesEverythingWhereSCPReadsIt(t *testing.T) {
	bridge := scpBridgeArgs(45678, "/state/id_ed25519", "/tmp/known_hosts")

	t.Run("upload", func(t *testing.T) {
		args := scpArgs(scpInvocation{
			bridge:   bridge,
			options:  []string{"-r"},
			operands: []string{"./dist", "sbx_devbox00000001@127.0.0.1:/tmp/dist"},
			remote:   []bool{false, true},
		})
		joined := strings.Join(args, " ")
		for _, want := range []string{"-P 45678", "-i /state/id_ed25519", "-F none", "-r -- ./dist sbx_devbox00000001@127.0.0.1:/tmp/dist"} {
			if !strings.Contains(joined, want) {
				t.Errorf("args %v missing %q", args, want)
			}
		}
		if strings.Contains(joined, " -3") {
			t.Errorf("a copy with a local end must not be routed with -3: %v", args)
		}
	})

	t.Run("discobox to discobox", func(t *testing.T) {
		args := scpArgs(scpInvocation{
			bridge:   bridge,
			operands: []string{"sbx_a@127.0.0.1:/tmp/a", "sbx_b@127.0.0.1:/tmp/a"},
			remote:   []bool{true, true},
		})
		// The direct remote-to-remote path has the source sandbox dial the
		// destination, which is a loopback port that exists only on this
		// machine. -3 is pinned so no client default can take it.
		if !slices.Contains(args, "-3") {
			t.Fatalf("args %v missing -3", args)
		}
	})

	t.Run("two downloads are not a remote-to-remote copy", func(t *testing.T) {
		args := scpArgs(scpInvocation{
			bridge:   bridge,
			operands: []string{"sbx_a@127.0.0.1:/tmp/a", "sbx_b@127.0.0.1:/tmp/b", "./here"},
			remote:   []bool{true, true, false},
		})
		if slices.Contains(args, "-3") {
			t.Fatalf("a local destination needs no -3: %v", args)
		}
	})
}
