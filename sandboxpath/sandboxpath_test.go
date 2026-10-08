package sandboxpath

import (
	"testing"

	"github.com/discobox-ai/discobox/platform"
)

var (
	linux   = For(platform.Platform{OS: "linux", Arch: "amd64"})
	darwin  = For(platform.Platform{OS: "darwin", Arch: "arm64"})
	windows = For(platform.Platform{OS: "windows", Arch: "amd64"})
)

func TestWorkingRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		want  string
	}{
		{"linux", linux, "/workspace"},
		{"darwin", darwin, "/workspace"},
		{"windows", windows, `C:\workspace`},
	} {
		if got := tc.paths.WorkingRoot(); got != tc.want {
			t.Errorf("%s: WorkingRoot() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestVolumesAreLinuxOnly(t *testing.T) {
	if !linux.Volumes() {
		t.Error("linux has no volumes")
	}
	if darwin.Volumes() || windows.Volumes() {
		t.Error("a non-Linux sandbox has volumes")
	}
	if For(platform.Platform{}).Volumes() {
		t.Error("an undeclared platform has volumes")
	}
}

func TestIsAbs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		value string
		want  bool
	}{
		{"linux root", linux, "/", true},
		{"linux absolute", linux, "/home/ada/.cache", true},
		{"linux relative", linux, "home/ada", false},
		{"linux drive is relative", linux, `C:\Users\ada`, false},
		{"darwin absolute", darwin, "/Users/ada", true},
		{"darwin relative", darwin, "./src", false},
		{"windows drive", windows, `C:\Users\ada`, true},
		{"windows drive, slashes", windows, "c:/Users/ada", true},
		{"windows drive root", windows, `D:\`, true},
		{"windows share", windows, `\\server\share\src`, true},
		{"windows share, slashes", windows, "//server/share", true},
		{"windows drive-relative", windows, `C:src`, false},
		{"windows bare drive", windows, `C:`, false},
		{"windows rooted, no drive", windows, `\Users\ada`, false},
		{"windows POSIX path", windows, "/workspace", false},
		{"windows server, no share", windows, `\\server`, false},
		{"windows relative", windows, `src\app`, false},
	} {
		if got := tc.paths.IsAbs(tc.value); got != tc.want {
			t.Errorf("%s: IsAbs(%q) = %v, want %v", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestClean(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		value string
		want  string
	}{
		{"linux", linux, "/workspace//a/./b/../c/", "/workspace/a/c"},
		{"linux above root", linux, "/../x", "/x"},
		{"linux backslash is a name", linux, `/a\b`, `/a\b`},
		{"darwin", darwin, "/Users/ada/../bob/", "/Users/bob"},
		{"windows separators", windows, "C:/workspace/a//b/", `C:\workspace\a\b`},
		{"windows dots", windows, `C:\a\.\b\..\c`, `C:\a\c`},
		{"windows above drive", windows, `C:\..\..\x`, `C:\x`},
		{"windows drive root", windows, `C:/`, `C:\`},
		{"windows bare drive", windows, `C:`, `C:`},
		{"windows drive-relative", windows, `C:a\..\b`, `C:b`},
		{"windows share", windows, `\\srv\sh\a\..\..\b`, `\\srv\sh\b`},
		{"windows share alone", windows, `\\srv\sh`, `\\srv\sh\`},
		{"windows relative", windows, `a/b/../c`, `a\c`},
	} {
		if got := tc.paths.Clean(tc.value); got != tc.want {
			t.Errorf("%s: Clean(%q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestJoin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		elem  []string
		want  string
	}{
		{"linux", linux, []string{"/workspace", "source"}, "/workspace/source"},
		{"darwin", darwin, []string{"/workspace", "", "a/../b"}, "/workspace/b"},
		{"windows", windows, []string{`C:\workspace`, "source"}, `C:\workspace\source`},
		{"windows slashes in an element", windows, []string{`C:\workspace`, "a/b"}, `C:\workspace\a\b`},
		{"nothing", linux, []string{"", ""}, ""},
	} {
		if got := tc.paths.Join(tc.elem...); got != tc.want {
			t.Errorf("%s: Join(%q) = %q, want %q", tc.name, tc.elem, got, tc.want)
		}
	}
}

func TestDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		value string
		want  string
	}{
		{"linux", linux, "/workspace/source", "/workspace"},
		{"linux root", linux, "/", "/"},
		{"darwin", darwin, "/Users/ada/src/", "/Users/ada/src"},
		{"windows", windows, `C:\workspace\source`, `C:\workspace`},
		{"windows slashes", windows, "C:/workspace/source", `C:\workspace`},
		{"windows drive root", windows, `C:\`, `C:\`},
		{"windows top level", windows, `C:\workspace`, `C:\`},
		{"windows share", windows, `\\srv\sh\src`, `\\srv\sh\`},
		{"windows relative", windows, `a\b`, `a`},
	} {
		if got := tc.paths.Dir(tc.value); got != tc.want {
			t.Errorf("%s: Dir(%q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestRooted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths Paths
		value string
		want  string
	}{
		{"linux", linux, "  /workspace/src/ ", "/workspace/src"},
		{"linux relative is read from the root", linux, "workspace/src", "/workspace/src"},
		{"linux doubled root", linux, "//workspace", "/workspace"},
		{"linux root", linux, "/", ""},
		{"linux root by dots", linux, "/a/..", ""},
		{"linux whitespace inside", linux, "/work space", ""},
		{"linux blank", linux, "  ", ""},
		{"linux drive is a name under the root", linux, `C:\src`, `/C:\src`},
		{"darwin relative", darwin, "src", "/src"},
		{"darwin", darwin, "/Users/ada/src", "/Users/ada/src"},
		{"windows", windows, "c:/src/app", `c:\src\app`},
		{"windows drive root", windows, `C:\`, ""},
		{"windows share root", windows, `\\srv\sh`, ""},
		{"windows share", windows, `\\srv\sh\src`, `\\srv\sh\src`},
		{"windows POSIX path", windows, "/workspace", ""},
		{"windows drive-relative", windows, `C:src`, ""},
	} {
		if got := tc.paths.Rooted(tc.value); got != tc.want {
			t.Errorf("%s: Rooted(%q) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}
}
