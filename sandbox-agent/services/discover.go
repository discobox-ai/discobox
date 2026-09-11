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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/discobox-ai/discobox/sandboxservices"
	"github.com/discobox-ai/x/frontmatter"
)

// DirName is where services are declared, relative to the sandbox's primary
// source directory. It sits beside `.discobox/hooks` and shares its file
// format (see frontmatter).
const DirName = ".discobox/services"

// BuiltinDir is where the image declares the services it ships, in the same
// format. It sits beside the image's skills directory and is read the same way
// (ADR 0080's pairing, extended to image-declared services by ADR 0094).
//
// The desktop viewer is what it exists for. Its port is bound by a `.socket`
// unit, so the port watcher's uid filter cannot see it, and classifying it
// would mean connecting to it — which is what socket activation *is*, so a
// classification probe would start an X server, a window manager and a VNC
// server in every sandbox on a timer. A declaration states the port and the
// protocol, and the port is then reported without ever being touched.
const BuiltinDir = "/usr/local/share/discobox/services"

// idPattern is what an explicit `id:` may look like: lowercase reverse-DNS.
// Deliberately narrower than what a filename normalizes to, because an id
// written by hand is a name other things match on, and case or punctuation
// differences would be invisible reasons for a match to fail.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$`)

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
	// Ports are the TCP ports this service serves, in the order the file names
	// them, empty when it names none.
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
	// activation (ADR 0094, image-declared services).
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
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []Definition
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		def := parseFile(filepath.Join(dir, name), name)
		if def.ID == "" {
			continue
		}
		def.Builtin = builtin
		// The reserved namespace belongs to the image. Enforced here rather
		// than in parseFile because only the caller knows which directory the
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
			def.ID = frontmatter.NormalizeID(name)
		}
		// Two files whose names normalize to one id would otherwise take turns
		// being "the" service depending on directory order, and stopping one
		// would stop whichever the last listing happened to resolve to.
		if first, ok := seen[def.ID]; ok {
			def.Problem = fmt.Sprintf("service id %q is already declared by %s", def.ID, first)
		} else {
			seen[def.ID] = name
		}
		out = append(out, def)
	}
	// Filename order, so the `NN-` prefix does what it looks like it does.
	sort.Slice(out, func(i, j int) bool { return out[i].FileName < out[j].FileName })
	return out, nil
}

func parseFile(path, filename string) Definition {
	def := Definition{
		ID:       frontmatter.NormalizeID(filename),
		Name:     frontmatter.DefaultName(filename),
		Path:     path,
		FileName: filename,
	}
	if def.ID == "" {
		return def
	}
	data, err := os.ReadFile(path)
	if err != nil {
		def.Problem = err.Error()
		return def
	}
	parsed, err := frontmatter.Parse(data)
	if err != nil {
		def.Problem = err.Error()
		return def
	}
	fields, err := frontmatter.Decode(parsed.Meta)
	if err != nil {
		def.Problem = "front matter: " + err.Error()
		return def
	}
	// An explicit id replaces the filename-derived one, and is not normalized:
	// NormalizeID turns a dot into a dash, which would quietly make
	// `ai.discobox.desktop` into `ai-discobox-desktop` and break every match on
	// it. It is validated instead.
	if id := strings.TrimSpace(firstField(fields, "id")); id != "" {
		if !idPattern.MatchString(id) {
			def.Problem = fmt.Sprintf("front matter: id: %q is not a lowercase reverse-DNS identifier", id)
			return def
		}
		def.ID = id
	}
	if name := fields.String("name"); name != "" {
		def.Name = name
	}
	def.Description = fields.String("description")
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
	protocol := strings.ToLower(strings.TrimSpace(firstField(fields, "protocol")))
	switch protocol {
	case "", "http", "https", "tcp":
		def.Protocol = protocol
	default:
		def.Problem = fmt.Sprintf("front matter: protocol: %q is not http, https or tcp", protocol)
		return def
	}

	switch mode := StartMode(strings.ToLower(strings.TrimSpace(firstField(fields, "start")))); mode {
	case "", StartCommand:
		def.Start = StartCommand
	case StartNever:
		def.Start = StartNever
	default:
		def.Problem = fmt.Sprintf("front matter: start: %q is not command or never", mode)
		return def
	}
	// A declaration that starts nothing is not a script, so the two things that
	// make a script runnable are not asked of it.
	if def.Start != StartNever {
		def.Problem = validate(path, data)
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
// A value that is not a TCP port number is an error rather than a skipped
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
				return nil, fmt.Errorf("ports: %q is not a TCP port number", text)
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

// validate holds a service script to the same two rules a hook script is held
// to, and for the same reason: the file is run by path, so the kernel needs a
// shebang to know what to run it with and the bit to be allowed to.
func validate(path string, data []byte) string {
	if !frontmatter.HasShebangLine(data) {
		return "script must start with a shebang line"
	}
	if runtime.GOOS == "windows" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	if info.Mode()&0o111 == 0 {
		return "script is not executable"
	}
	return ""
}
