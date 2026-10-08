// Package sandboxpath judges a path inside a sandbox by that sandbox's own
// platform (ADR 0145 §6), never by the host the judging code runs on and never
// as a Linux path by default.
//
// A path inside a sandbox is named by every component on the way to it: the
// CLI places sources and reads a user's home, pool-agent cleans what a create
// request names and writes the working root into the manifest, and the sandbox
// agent seeds that root and wires the image's declared volumes. Each runs on a
// host of its own — the CLI on Windows, the server on macOS, pool-agent in a
// Linux container — so neither `path/filepath` (the host's rules) nor `path`
// (always POSIX) is right for all of them. They take a Paths for the sandbox's
// platform instead, so every end judges one path the same way.
//
// In the root module because the CLI, the server, pool-agent and the sandbox
// agent all name sandbox paths and must agree on them.
package sandboxpath

import (
	"path"
	"strings"

	"github.com/discobox-ai/discobox/platform"
)

// Paths is how paths are spelled and judged inside a sandbox of one platform.
// Linux and macOS are POSIX: one root, '/' separators. Windows paths begin
// with a drive (`C:\`) or a share (`\\server\share`), and either separator is
// accepted on the way in; a Paths for Windows writes back slashes.
type Paths struct {
	os      string
	windows bool
	linux   bool
}

// For is the path rules of a sandbox of platform p. Only the OS matters: an
// architecture spells paths no differently.
func For(p platform.Platform) Paths {
	return Paths{os: p.OS, windows: p.OS == "windows", linux: p.OS == "linux"}
}

// OS is the sandbox platform's OS these rules are for, as GOOS spells it, for
// a message to name.
func (p Paths) OS() string { return p.os }

// workingRoot is the directory a sandbox works in when its manifest names
// none. It is the same directory on every POSIX platform: a source placed under
// it, or a host path mirrored to it, means the same thing whichever platform
// the sandbox is. On macOS the root volume is read-only, so the template makes
// /workspace a synthetic firmlink onto its data volume (synthetic.conf); that
// is the template's job, not a reason for the path to differ.
const (
	posixWorkingRoot   = "/workspace"
	windowsWorkingRoot = `C:\workspace`
)

// WorkingRoot is the directory a sandbox works in when its manifest names
// none. It is one answer per platform because four components have to agree on
// it: pool-agent writes it into the manifest and places sources under it, the
// sandbox agent falls back to it when reading a manifest an older pool agent
// wrote, boot creates it and gives it to the sandbox user, and the CLI derives
// the source destinations it asks for from it. A sandbox working in a directory
// boot never chowned, or beside a checkout the CLI put under another root, is
// the failure that disagreement produces — and the CLI's is the one nothing
// server-side can correct, because a destination it names is explicit in the
// create request and overrides pool-agent's own default.
func (p Paths) WorkingRoot() string {
	if p.windows {
		return windowsWorkingRoot
	}
	return posixWorkingRoot
}

// Volumes reports whether a sandbox of this platform has declared volumes at
// all. They are a container mechanism (ADR 0007): they wire an image's paths
// onto primary volumes a pool mounted. A VM sandbox — every non-Linux one — has
// neither; its disk is its data and its cache a directory on that disk, so
// there is nothing to bind, and a declaration is invalid rather than ignored.
// The `%UID%` and `%GID%` tokens appear only in a volume declaration, so they
// have meaning exactly where volumes do.
func (p Paths) Volumes() bool { return p.linux }

// IsAbs reports whether value is an absolute path in the sandbox. On Windows
// that is a drive and a separator (`C:\x`) or a share (`\\server\share`); a
// path that begins with a separator alone is relative to whatever drive is
// current, which names no one place.
func (p Paths) IsAbs(value string) bool {
	if !p.windows {
		return strings.HasPrefix(value, "/")
	}
	volume, rest := splitWindowsVolume(value)
	if volume == "" {
		return false
	}
	if strings.HasPrefix(volume, `\\`) {
		return true
	}
	return rest != "" && isWindowsSeparator(rest[0])
}

// Clean is the shortest spelling of value by purely lexical processing, the
// way path.Clean is: separators collapsed, `.` dropped, `..` resolved where it
// can be. On Windows, `/` becomes `\` and a `..` never climbs above the drive.
func (p Paths) Clean(value string) string {
	if !p.windows {
		return path.Clean(value)
	}
	volume, rest := splitWindowsVolume(value)
	rest = strings.ReplaceAll(rest, `\`, "/")
	switch {
	case rest == "" && volume != "" && !strings.HasPrefix(volume, `\\`):
		// "C:" is the current directory on drive C, and stays that.
		return volume
	case strings.HasPrefix(rest, "/"):
		rest = path.Clean(rest)
	default:
		rest = path.Clean(rest)
		if strings.HasPrefix(volume, `\\`) {
			// A share always has a root after it.
			rest = path.Clean("/" + rest)
		}
	}
	return volume + strings.ReplaceAll(rest, "/", `\`)
}

// Join joins elements with this platform's separator and cleans the result,
// ignoring empty ones, the way path.Join does.
func (p Paths) Join(elem ...string) string {
	sep := "/"
	if p.windows {
		sep = `\`
	}
	parts := make([]string, 0, len(elem))
	for _, e := range elem {
		if e != "" {
			parts = append(parts, e)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return p.Clean(strings.Join(parts, sep))
}

// Dir is every element of value but the last, cleaned, the way path.Dir is.
// A root is its own directory.
func (p Paths) Dir(value string) string {
	if !p.windows {
		return path.Dir(value)
	}
	volume, rest := splitWindowsVolume(value)
	rest = strings.ReplaceAll(rest, `\`, "/")
	i := strings.LastIndex(rest, "/")
	return p.Clean(volume + rest[:i+1])
}

// Rooted is value as a path below the root, cleaned, or "" when it names no
// such place. It is the reading a create request's sandbox path gets — a
// source's directory, the user's home, a working directory: surrounding space
// is trimmed, and a path with whitespace inside it, or one naming the root
// itself, names nothing a sandbox may be given.
//
// A relative path is read from the root on a platform that has one, so on
// Linux and macOS "src" is "/src", as pool-agent has always read it. Windows
// has a root per drive and none of them is the root, so there a path that does
// not say its drive or share names no place.
func (p Paths) Rooted(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return ""
	}
	if !p.windows {
		value = "/" + value
	} else if !p.IsAbs(value) {
		return ""
	}
	cleaned := p.Clean(value)
	if p.isRoot(cleaned) {
		return ""
	}
	return cleaned
}

// isRoot reports whether a cleaned absolute path is a root: "/", a drive's
// `C:\`, or a share's `\\server\share\`.
func (p Paths) isRoot(cleaned string) bool {
	if !p.windows {
		return cleaned == "/"
	}
	volume, rest := splitWindowsVolume(cleaned)
	return rest == `\` || (strings.HasPrefix(volume, `\\`) && rest == "")
}

// splitWindowsVolume splits a Windows path into its volume — `C:`, or
// `\\server\share` for a share — and the rest. A path with neither has an
// empty volume. Either separator is read as one, so a path spelled with
// forward slashes is the same path.
func splitWindowsVolume(value string) (volume, rest string) {
	if len(value) >= 2 && value[1] == ':' && isDriveLetter(value[0]) {
		return value[:2], value[2:]
	}
	if len(value) >= 2 && isWindowsSeparator(value[0]) && isWindowsSeparator(value[1]) {
		// \\server\share: the server and the share are both part of the
		// volume, and neither may be empty.
		tail := value[2:]
		server, afterServer, ok := cutWindowsSeparator(tail)
		if !ok || server == "" {
			return "", value
		}
		share, afterShare, hasRest := cutWindowsSeparator(afterServer)
		if share == "" {
			return "", value
		}
		volume = `\\` + server + `\` + share
		if !hasRest {
			return volume, ""
		}
		return volume, `\` + afterShare
	}
	return "", value
}

func cutWindowsSeparator(value string) (before, after string, found bool) {
	if i := strings.IndexAny(value, `\/`); i >= 0 {
		return value[:i], value[i+1:], true
	}
	return value, "", false
}

func isDriveLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isWindowsSeparator(c byte) bool { return c == '\\' || c == '/' }
