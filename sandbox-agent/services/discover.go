// Package services runs the sandbox's declared services: the scripts under
// `.discobox/services` that the sandbox starts for you at boot and that
// `discobox admin services` and the workspace act on afterwards.
//
// They come from two places, and the format is the same in both. The
// repository declares its own under `.discobox/services`, and the image
// declares what it ships under BuiltinDir — the same pairing `.discobox/skills`
// has with the image's skill directory (ADR 0080), for the same reason: some
// services are true of the sandbox whatever repository is being worked on in
// it, and cannot come from the repository being worked on.
//
// A service is an exec (ADR 0070). This package owns the declaration — where
// it is read from, what it may say, and what makes one invalid — and the
// mapping from a declaration to the exec running it; every runtime mechanic
// (units, shims, logs, transcripts, status) belongs to execs.Manager.
package services

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/discobox-ai/discobox/declared"
	"github.com/discobox-ai/discobox/sandboxservices"
	"github.com/discobox-ai/x/frontmatter"
)

// DirName is where services are declared, relative to the sandbox's primary
// source directory. It sits beside `.discobox/hooks` and shares its file
// format (see frontmatter).
const DirName = ".discobox/services"

// BuiltinDir is where the image declares the services it ships, in the same
// format. It sits beside the image's skills directory and is read the same way
// (ADR 0080's pairing, extended to image-declared services by ADR 26-09-05-409).
//
// The desktop viewer is what it exists for. Its port is bound by a `.socket`
// unit, so the port watcher's uid filter cannot see it, and classifying it
// would mean connecting to it — which is what socket activation *is*, so a
// classification probe would start an X server, a window manager and a VNC
// server in every sandbox on a timer. A declaration states the port and the
// protocol, and the port is then reported without ever being touched.
const BuiltinDir = "/usr/local/share/discobox/services"

// StartMode says who starts a service.
type StartMode string

const (
	// StartCommand is the default: the script is the service, and the sandbox
	// runs it.
	StartCommand StartMode = "command"
	// StartNever declares ports without declaring anything to run. Something
	// outside this package already serves them — a systemd socket unit, a
	// nested container — and the declaration exists so they are reported and
	// forwarded rather than started.
	//
	// It is not a broken script. A declaration with nothing to run has no
	// shebang and no executable bit, and both of those are Problems for a
	// service the sandbox is meant to launch, so saying so outright is what
	// keeps the desktop from being listed as permanently broken.
	StartNever StartMode = "never"
)

// Definition is one declared service, as its file says it.
//
// A definition with a Problem is still a definition: it is listed, with the
// reason it cannot run, rather than dropped. A service that silently fails to
// appear is indistinguishable from one nobody declared, and the usual cause —
// a missing executable bit — is invisible in an editor.
type Definition struct {
	// ID is the identity every command addresses a service by. It is derived
	// from the filename — `10-discobox-api.sh` is `discobox-api` — unless the
	// declaration names one with `id:`.
	//
	// A filename-derived id is fine for a repository's own services, where the
	// file and the name move together. It is not enough for a service a client
	// has to *recognize*: renaming the file changes it, and any repository can
	// declare `desktop`. A declaration that something outside the sandbox keys
	// off states its id outright, in reverse-DNS form, and the DiscoboxIDPrefix
	// namespace is reserved for the ones Discobox itself ships.
	ID string
	// Name is the display name, defaulted from the filename.
	Name string
	// Description is what the service is for, empty when it declared none.
	Description string
	// Path is the absolute path to the script inside the sandbox, which is
	// what actually gets run.
	Path string
	// FileName is the file's own name, kept because it — not the ID — is what
	// orders the listing: the `NN-` prefix stripped from the ID is a statement
	// about where the file sits in the directory.
	FileName string
	// Ports are the ports this service serves, in the order the file names
	// them, empty when it names none. They are TCP ports unless Protocol is
	// udp (ADR 0109 §2).
	//
	// A declared port is reported and forwarded whether or not anything is
	// observably listening on it (ADR 0076). It exists for the ports the
	// sandbox cannot discover on its own: discovery only sees sockets the
	// sandbox user owns, so a port published by a nested container or bound by
	// a socket-activated unit — root's socket either way — is invisible to it
	// however plainly the service is responsible for it.
	Ports []int
	// Protocol is what the declaration says its ports speak, empty when it
	// says nothing. Stated, it is reported instead of probing the port; that is
	// the whole point for a socket-activated service, where the probe is the
	// activation (ADR 26-09-05-409, image-declared services).
	//
	// It applies to every port the declaration names: a service serving two
	// ports that speak different things is two declarations.
	Protocol string
	// Start says who starts this service. Empty means StartCommand.
	Start StartMode
	// Builtin marks a declaration that came from the image rather than from the
	// repository being worked on.
	Builtin bool
	// Problem is why this declaration cannot run, empty when it can.
	Problem string
}

// Runnable reports whether this declaration is one the sandbox starts.
func (d Definition) Runnable() bool {
	return d.Problem == "" && d.Start != StartNever
}

// Discover reads both declaration directories: the image's first, then the
// repository's. A repository declaration wins on a shared id, the way its
// skills win on a shared name (ADR 0080) — a project working on the image's own
// desktop can say something different about it.
//
// Image-first ordering is also what settles a port two declarations name: the
// port watcher takes the first declaration of a port, so the image's stated
// protocol survives a repository service that happens to reuse the number
// without stating one.
//
// An absent directory is not an error — most repositories declare no services
// — and neither is a file that fails to parse: that becomes a Definition with
// a Problem. Only a directory that exists and cannot be read is an error, since
// then nothing can be said about what is declared.
func Discover(builtinDir, projectRoot string) ([]Definition, error) {
	builtin, err := discoverDir(builtinDir, true)
	if err != nil {
		return nil, err
	}
	project, err := discoverDir(filepath.Join(projectRoot, DirName), false)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(builtin)+len(project))
	overridden := map[string]struct{}{}
	for _, def := range project {
		overridden[def.ID] = struct{}{}
	}
	for _, def := range builtin {
		if _, replaced := overridden[def.ID]; replaced {
			continue
		}
		out = append(out, def)
	}
	return append(out, project...), nil
}

// discoverDir reads one directory of declarations. A missing directory is
// nothing declared, not an error: almost every repository has none, and an
// image built without the desktop ships none.
func discoverDir(dir string, builtin bool) ([]Definition, error) {
	files, err := declared.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(files))
	for _, file := range files {
		def := definition(file)
		def.Builtin = builtin
		// The reserved namespace belongs to the image. Enforced here rather
		// than in definition because only the caller knows which directory the
		// file came out of.
		if !builtin && def.Problem == "" && sandboxservices.Reserved(def.ID) {
			def.Problem = fmt.Sprintf("front matter: id: %q is reserved for services the image declares", def.ID)
			// And the claim is dropped, not merely reported: the declaration
			// falls back to its filename-derived id, exactly as one with a
			// malformed `id:` does. Leaving the reserved id on a refused
			// definition is the takeover this check exists to stop — Discover
			// would let it override the image's own declaration, and
			// Manager.Declarations, which publishes a declaration's ports
			// whether or not it can run, would put the repository's port on
			// the wire under the image's id. NormalizeID turns a dot into a
			// dash, so a filename can never produce a reserved id.
			def.ID = frontmatter.NormalizeID(file.FileName)
		}
		out = append(out, def)
	}
	return out, nil
}

// definition reads what one declaration file says about a service. The shape
// of the file — id, name, a duplicate, a malformed block — is declared's to
// judge; what is left is what a service's fields mean.
func definition(file declared.File) Definition {
	def := Definition{
		ID:          file.ID,
		Name:        file.Name,
		Description: file.Description,
		Path:        file.Path,
		FileName:    file.FileName,
		Problem:     file.Problem,
	}
	if def.Problem != "" {
		return def
	}
	fields := file.Fields
	ports, err := parsePorts(fields)
	if err != nil {
		def.Problem = "front matter: " + err.Error()
		return def
	}
	def.Ports = ports

	// A stated protocol replaces the port watcher's probe rather than
	// confirming it, so an unrecognized value is an error rather than a field
	// quietly ignored: the difference between them is whether a
	// socket-activated service gets started by classification.
	protocol := strings.ToLower(firstField(fields, "protocol"))
	switch protocol {
	case "", "http", "https", "tcp", "udp":
		def.Protocol = protocol
	default:
		def.Problem = fmt.Sprintf("front matter: protocol: %q is not http, https, tcp or udp", protocol)
		return def
	}

	// A .yaml declaration is metadata with no script in it (ADR 0125 §1), so
	// there is nothing it could start: saying nothing means never, and saying
	// command is a contradiction worth reporting.
	switch mode := StartMode(strings.ToLower(firstField(fields, "start"))); {
	case mode == "" && file.Metadata, mode == StartNever:
		def.Start = StartNever
	case mode == StartCommand && file.Metadata:
		def.Problem = "start: command needs a script to run, and a .yaml declaration has none"
		return def
	case mode == "", mode == StartCommand:
		def.Start = StartCommand
	default:
		def.Problem = fmt.Sprintf("front matter: start: %q is not command or never", mode)
		return def
	}
	// A declaration that starts nothing is not a script, so the two things that
	// make a script runnable are not asked of it.
	if def.Start != StartNever {
		def.Problem = declared.ScriptProblem(file)
	}
	return def
}

// firstField is the first value of the first name that appears, empty when
// none does. The front matter is a multimap, and every field read here takes
// exactly one value.
func firstField(fields frontmatter.Fields, names ...string) string {
	for _, value := range fields.Strings(names...) {
		return value
	}
	return ""
}

// parsePorts reads the `ports` field, which a declaration may write as a YAML
// list, as one comma- or space-separated scalar, or as a single number —
// `ports: 8080`, `ports: 8080, 5432` and `ports: [8080, 5432]` all say the same
// thing. `port` is accepted as the singular spelling of the same field, because
// a file declaring one port will be written that way whatever the docs say.
//
// A value that is not a port number is an error rather than a skipped
// entry: the author meant something specific by it, and a service that runs
// without the port it declared is the invisible failure this package's listing
// already refuses elsewhere.
func parsePorts(fields frontmatter.Fields) ([]int, error) {
	var out []int
	seen := map[int]struct{}{}
	for _, field := range fields.Strings("ports", "port") {
		for _, text := range strings.FieldsFunc(field, isPortSeparator) {
			port, err := strconv.Atoi(text)
			if err != nil || port < 1 || port > 65535 {
				return nil, fmt.Errorf("ports: %q is not a port number (a declaration's ports are all TCP or, with protocol: udp, all UDP)", text)
			}
			if _, duplicate := seen[port]; duplicate {
				continue
			}
			seen[port] = struct{}{}
			out = append(out, port)
		}
	}
	return out, nil
}

func isPortSeparator(r rune) bool { return r == ',' || unicode.IsSpace(r) }
