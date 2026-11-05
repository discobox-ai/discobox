package cli

import (
	"runtime"
	"testing"
)

func TestCopyOptions(t *testing.T) {
	for _, flag := range []string{"-O", "-S", "-i", "-J", "-P", "-F", "-o", "-C", "-v", "--recursive"} {
		if _, _, err := parseCopyArgs([]string{flag, "a", "box:b"}); err == nil {
			t.Fatalf("accepted unsupported %s", flag)
		}
	}
	opts, paths, err := parseCopyArgs([]string{"-rpq", "--", "-literal", "box:a"})
	if err != nil || !opts.recursive || !opts.preserve || !opts.quiet || len(paths) != 2 || paths[0] != "-literal" {
		t.Fatalf("parse: %+v %v %v", opts, paths, err)
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
		// A bare colon is the only discobox or a pick, the one place this differs
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
