package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

// A built-in harness is authored as one declaration, harness/<directory>/
// harness.json, and one vendor adapter beside it (ADR 26-11-06-596). The
// declaration names every part of the harness, who owns it and when it runs;
// the bundle a box applies is generated from it (internal/harnessbundle) and
// is the only published contract. Third-party bundles never see this format.
const (
	// DeclarationFile is a built-in's declaration, in its directory.
	DeclarationFile = "harness.json"
	// AdapterFile is a built-in's vendor operations, in its directory.
	AdapterFile = "adapter.ts"
	// DeclarationSchema is the one declaration schema this build reads.
	DeclarationSchema = 1
)

// Declaration is a built-in harness's harness.json. Every part is either
// configured or declared absent with a reason ("none"), never inferred from
// which files a directory holds.
type Declaration struct {
	Schema int `json:"schema"`

	// Identity. ID is the harness ID and its built-in config's slug; Bundle
	// the bundle name it is published as. The directory and the hook
	// provider are distinct identities of their own (Directory, Hooks).
	ID                 string `json:"id"`
	Bundle             string `json:"bundle"`
	Name               string `json:"name"`
	Description        string `json:"description,omitempty"`
	PlatformAPIVersion string `json:"platformAPIVersion"`

	Install     Install     `json:"install"`
	Launch      Launch      `json:"launch"`
	Environment Environment `json:"environment"`
	Settings    Settings    `json:"settings"`
	LocalState  LocalState  `json:"localState"`
	Credentials Credentials `json:"credentials"`
	Hooks       Hooks       `json:"hooks"`
	Capture     Capture     `json:"capture"`

	// Directory is the harness/<directory> the declaration was read from.
	Directory string `json:"-"`
}

// Install is the machine-scoped CLI a harness's feature provisions. Every
// built-in agent comes from npm through the platform's npm-global helper, so
// a package and the command it puts on PATH are all an install says; the same
// two facts are the pool-cached version store's (ADR 0114 §7).
type Install struct {
	None string `json:"none,omitempty"`
	// Requires are the platform features the CLI runs on; core always, whose
	// no-op discobox-harness-run the launcher replaces.
	Requires   []string `json:"requires,omitempty"`
	Package    string   `json:"package,omitempty"`
	Executable string   `json:"executable,omitempty"`
	// OSPackages are distribution packages the CLI looks for on PATH.
	OSPackages *OSPackages `json:"osPackages,omitempty"`
	// Env is set for the install and, through the manifest, for the
	// version store's later installs of another version.
	Env map[string]string `json:"env,omitempty"`
}

// OSPackages are distribution packages, by distribution.
type OSPackages struct {
	Debian []string `json:"debian,omitempty"`
}

// Launch is how the shared launcher turns `discobox-harness-run [--resume]
// <prompt words>` into the CLI's argv.
type Launch struct {
	None string `json:"none,omitempty"`
	// Args are always passed, before the prompt or resume arguments.
	Args []string `json:"args,omitempty"`
	// Resume replaces the prompt on a relaunch (ADR 0086 §4).
	Resume []string `json:"resume,omitempty"`
	Prompt Prompt   `json:"prompt"`
	// Home is the CLI's own state directory, for the strategies below that
	// name paths in it.
	Home *Home `json:"home,omitempty"`
	// Retry starts the CLI again when it fails before a session began.
	Retry *Retry `json:"retry,omitempty"`
}

// Prompt is how the joined prompt reaches the CLI.
type Prompt struct {
	// Form is positional ("<prompt>"), separator ("-- <prompt>"), option
	// ("<option> <prompt>") or option-joined ("<option>=<prompt>").
	Form   string `json:"form"`
	Option string `json:"option,omitempty"`
	// LeadingAt "space" sends a prompt starting with @ with a space before
	// it, for a CLI that reads such an argument as a file even after --.
	LeadingAt string `json:"leadingAt,omitempty"`
}

// Home is a directory the CLI keeps its state in: Env when set, otherwise
// Default under the user's home.
type Home struct {
	Env     string `json:"env,omitempty"`
	Default string `json:"default"`
}

// Retry restarts a CLI that exited with Status before a session file
// appeared or grew under Sessions (relative to Launch.Home), after each delay
// in turn.
type Retry struct {
	Status   int    `json:"status"`
	Delays   []int  `json:"delays"`
	Sessions string `json:"sessions"`
}

// Environment is the environment the CLI runs in.
type Environment struct {
	// Static is the manifest's env.
	Static map[string]string `json:"static,omitempty"`
	// Defaults are set by the launcher when unset or empty.
	Defaults map[string]string `json:"defaults,omitempty"`
	// Aliases copy one variable to another when the other is unset, in the
	// launcher and, when Profile names a script, every login shell.
	Aliases []Alias `json:"aliases,omitempty"`
	Profile string  `json:"profile,omitempty"`
	// FromSettings is a Discobox settings file in the home the adapter's
	// launchEnvironment derives variables from at every launch.
	FromSettings string `json:"fromSettings,omitempty"`
	// Volumes are the manifest's volumes.
	Volumes []Volume `json:"volumes,omitempty"`
}

// Alias sets Name from From, preserving a Name already set.
type Alias struct {
	Name string `json:"name"`
	From string `json:"from"`
}

// Settings are the harness's two kinds of configuration: portable seed files
// in the home, which a saved harness change replaces, and machine policy,
// which nothing a user saves outranks or removes.
type Settings struct {
	// Files are the manifest's harness files, verbatim (File).
	Files json.RawMessage `json:"files,omitempty"`
	// Policy assets are installed at provisioning.
	Policy []Policy `json:"policy,omitempty"`
}

// Policy is a literal asset installed at a fixed path.
type Policy struct {
	Asset string `json:"asset"`
	Path  string `json:"path"`
	// Layer names the vendor's precedence for the reader: "managed" outranks
	// the user's settings, "system" sits below them, "overlay" is named by
	// the manifest's env.
	Layer string `json:"layer"`
}

// LocalState is the sandbox-local state the launcher prepares.
type LocalState struct {
	Trust  Trust  `json:"trust"`
	Memory Memory `json:"memory"`
}

// Trust is how a folder the CLI starts in is trusted.
type Trust struct {
	// Strategy is "none", "local-state" (the adapter's prepareLocalState,
	// under the capture lock) or "system-layer" (the adapter's systemTrust
	// rewrites Layer as root, under Lock).
	Strategy string `json:"strategy"`
	None     string `json:"none,omitempty"`
	Layer    string `json:"layer,omitempty"`
	Lock     string `json:"lock,omitempty"`
}

// Memory is where the CLI's memories live: the primary source's durable
// data, partitioned by uid (ADR 0094).
type Memory struct {
	// Strategy is "none", "managed-setting" (Key in the JSON drop-in Path
	// names the directory) or "bind-mount" (the directory is bound onto
	// Target under Launch.Home).
	Strategy string `json:"strategy"`
	None     string `json:"none,omitempty"`
	// Name is the directory under harnesses/ in the source's data.
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Key    string `json:"key,omitempty"`
	Target string `json:"target,omitempty"`
}

// Credentials are what the harness signs in with.
type Credentials struct {
	// Secrets are the manifest's secrets, verbatim (Secret).
	Secrets     json.RawMessage `json:"secrets,omitempty"`
	EgressHosts []string        `json:"egressHosts,omitempty"`
	Store       Store           `json:"store"`
}

// Store is where the CLI keeps its login.
type Store struct {
	// Strategy is "none", "file" (Document is the CLI's own) or "native"
	// (the CLI keeps Database; Document is the delivered interchange file,
	// imported before launch and replaced through the CLI at handoff).
	Strategy string `json:"strategy"`
	None     string `json:"none,omitempty"`
	Document string `json:"document,omitempty"`
	Database string `json:"database,omitempty"`
	// ImportFailure is said when a native import fails; the CLI still
	// starts.
	ImportFailure string `json:"importFailure,omitempty"`
}

// Hooks is how the CLI's lifecycle reaches discobox-hook-publish.
type Hooks struct {
	None string `json:"none,omitempty"`
	// Provider is the name hooks are published under (ADR 0146).
	Provider string `json:"provider,omitempty"`
	// Transport is "command" (Format's configuration, generated at Path,
	// runs the publisher for each of Events), "extension" (Asset installed
	// at Path, loaded from HomeFile) or "plugin" (Asset, a directory,
	// installed at Path and named by a policy asset).
	Transport string   `json:"transport,omitempty"`
	Format    string   `json:"format,omitempty"`
	Path      string   `json:"path,omitempty"`
	Timeout   int      `json:"timeout,omitempty"`
	Events    []string `json:"events,omitempty"`
	Asset     string   `json:"asset,omitempty"`
	HomeFile  string   `json:"homeFile,omitempty"`
}

// Capture is the save-back definition (harnesscapture.Definition) without
// its commands, which are the generated capture entry point.
type Capture struct {
	None       string
	Definition harnesscapture.Definition
}

// UnmarshalJSON reads either {"none": reason} or a definition, strictly.
func (c *Capture) UnmarshalJSON(data []byte) error {
	var none struct {
		None string `json:"none"`
	}
	if err := strictJSON(data, &none); err == nil && none.None != "" {
		c.None = none.None
		return nil
	}
	return strictJSON(data, &c.Definition)
}

// MarshalJSON is UnmarshalJSON's inverse.
func (c Capture) MarshalJSON() ([]byte, error) {
	if c.None != "" {
		return json.Marshal(map[string]string{"none": c.None})
	}
	return json.Marshal(c.Definition)
}

// Coding reports whether the declaration installs an agent CLI: every
// built-in but the shell.
func (d *Declaration) Coding() bool { return d.Install.None == "" }

// CaptureCommand is the installed capture entry point, the command the
// save-back definition names (exportCommand, handoffCommand).
func (d *Declaration) CaptureCommand() string {
	return LibexecDir + "/" + d.ID + "-capture"
}

// SaveBack is the published save-back definition: the declared one, naming
// the generated capture command.
func (d *Declaration) SaveBack() *harnesscapture.Definition {
	if d.Capture.None != "" {
		return nil
	}
	def := d.Capture.Definition
	def.ExportCommand = []string{d.CaptureCommand(), "export"}
	def.HandoffCommand = []string{d.CaptureCommand(), "handoff"}
	return &def
}

// LibexecDir is where a harness's feature installs what only it runs.
const LibexecDir = "/usr/local/libexec/discobox"

// LoadDeclaration reads and validates directory's declaration from fsys, the
// harness tree (harness/), whose assets it checks.
func LoadDeclaration(fsys fs.FS, directory string) (*Declaration, error) {
	raw, err := fs.ReadFile(fsys, path.Join(directory, DeclarationFile))
	if err != nil {
		return nil, err
	}
	var d Declaration
	if err := strictJSON(raw, &d); err != nil {
		return nil, fmt.Errorf("harness %s: %s: %w", directory, DeclarationFile, err)
	}
	d.Directory = directory
	if err := d.Validate(fsys); err != nil {
		return nil, fmt.Errorf("harness %s: %w", directory, err)
	}
	return &d, nil
}

func strictJSON(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

var (
	slugPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	argPattern      = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	homePathPattern = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`)
	packagePattern  = regexp.MustCompile(`^(@[a-z0-9._-]+/)?[a-z0-9._-]+$`)
)

// Validate checks the declaration and every relationship between its parts
// that the generated bundle depends on. fsys is the harness tree.
func (d *Declaration) Validate(fsys fs.FS) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if d.Schema != DeclarationSchema {
		return fmt.Errorf("schema %d is not supported (want %d)", d.Schema, DeclarationSchema)
	}
	if !slugPattern.MatchString(d.ID) {
		fail("id %q is not a slug", d.ID)
	}
	if d.Bundle != "discobox-harness-"+d.ID {
		fail("bundle %q must be discobox-harness-%s", d.Bundle, d.ID)
	}
	if d.Name == "" || d.PlatformAPIVersion == "" {
		fail("name and platformAPIVersion are required")
	}
	assets := map[string]bool{}
	asset := func(what, name string) {
		if name == "" || path.Clean(name) != name || !strings.HasPrefix(name, "assets/") {
			fail("%s asset %q must be a clean path under assets/", what, name)
			return
		}
		if _, err := fs.Stat(fsys, path.Join(d.Directory, name)); err != nil {
			fail("%s asset %s: %v", what, name, err)
		}
		assets[name] = true
	}
	absolute := func(what, p string) {
		if !path.IsAbs(p) || path.Clean(p) != p {
			fail("%s %q must be a clean absolute path", what, p)
		}
	}
	homeRelative := func(what, p string) {
		if !homePathPattern.MatchString(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") {
			fail("%s %q must be a clean path in the home", what, p)
		}
	}

	if d.ID == ShellSlug {
		// The reserved login shell has no agent, launcher, hooks or capture:
		// a command typed into its terminal would be a second shell inside
		// the first (ADR 0086 §3).
		for part, none := range map[string]string{"install": d.Install.None, "launch": d.Launch.None, "hooks": d.Hooks.None, "capture": d.Capture.None} {
			if none == "" {
				fail("the shell harness declares %s; it must be none", part)
			}
		}
	}

	// Install.
	if d.Install.None == "" {
		if !packagePattern.MatchString(d.Install.Package) || !argPattern.MatchString(d.Install.Executable) {
			fail("install needs an npm package and an executable")
		}
		for _, need := range []string{"core", "node"} {
			if !slices.Contains(d.Install.Requires, need) {
				fail("install requires %s: the launcher replaces core's, and the shared runtime runs on node", need)
			}
		}
		if d.Install.OSPackages != nil {
			for _, p := range d.Install.OSPackages.Debian {
				if !packagePattern.MatchString(p) {
					fail("osPackages %q", p)
				}
			}
		}
		for name := range d.Install.Env {
			if !envNamePattern.MatchString(name) {
				fail("install env %q is not a variable name", name)
			}
		}
	} else if d.Install.Package != "" || d.Install.Executable != "" || len(d.Install.Requires) > 0 || d.Install.OSPackages != nil || len(d.Install.Env) > 0 {
		fail("install is none and declares an install")
	}

	// Launch.
	if d.Launch.None == "" {
		if !d.Coding() {
			fail("launch needs an install to run")
		}
		for _, a := range slices.Concat(d.Launch.Args, d.Launch.Resume) {
			if !argPattern.MatchString(a) {
				fail("launch argument %q has characters the launcher does not pass literally", a)
			}
		}
		if len(d.Launch.Resume) == 0 {
			fail("launch.resume is required: a relaunch continues the session")
		}
		switch d.Launch.Prompt.Form {
		case "positional", "separator":
			if d.Launch.Prompt.Option != "" {
				fail("prompt form %s takes no option", d.Launch.Prompt.Form)
			}
		case "option", "option-joined":
			if !argPattern.MatchString(d.Launch.Prompt.Option) || !strings.HasPrefix(d.Launch.Prompt.Option, "-") {
				fail("prompt form %s needs an option", d.Launch.Prompt.Form)
			}
		default:
			fail("prompt form %q is not positional, separator, option or option-joined", d.Launch.Prompt.Form)
		}
		if l := d.Launch.Prompt.LeadingAt; l != "" && l != "space" {
			fail("prompt leadingAt %q is not space", l)
		}
		if h := d.Launch.Home; h != nil {
			homeRelative("launch.home.default", h.Default)
			if h.Env != "" && !envNamePattern.MatchString(h.Env) {
				fail("launch.home.env %q", h.Env)
			}
		}
		if r := d.Launch.Retry; r != nil {
			if d.Launch.Home == nil || r.Status <= 0 || r.Status > 125 || len(r.Delays) == 0 {
				fail("launch.retry needs launch.home, a status and delays")
			}
			homeRelative("launch.retry.sessions", r.Sessions)
			for _, delay := range r.Delays {
				if delay <= 0 {
					fail("launch.retry delay %d", delay)
				}
			}
		}
	} else if d.Coding() {
		fail("launch is none for a harness that installs an agent")
	}

	// Environment.
	for name := range d.Environment.Static {
		if !envNamePattern.MatchString(name) {
			fail("environment.static %q", name)
		}
		if _, ok := d.Install.Env[name]; ok {
			fail("environment.static %q is install.env's too", name)
		}
	}
	for name, value := range d.Environment.Defaults {
		if !envNamePattern.MatchString(name) || strings.ContainsAny(value, "\n\x00") {
			fail("environment.defaults %q", name)
		}
	}
	for _, a := range d.Environment.Aliases {
		if !envNamePattern.MatchString(a.Name) || !envNamePattern.MatchString(a.From) || a.Name == a.From {
			fail("environment alias %+v", a)
		}
	}
	if d.Environment.Profile != "" {
		absolute("environment.profile", d.Environment.Profile)
		if len(d.Environment.Aliases) == 0 {
			fail("environment.profile with no aliases")
		}
	}
	if d.Environment.FromSettings != "" {
		homeRelative("environment.fromSettings", d.Environment.FromSettings)
	}
	if len(d.Environment.Defaults)+len(d.Environment.Aliases) > 0 || d.Environment.FromSettings != "" {
		if d.Launch.None != "" {
			fail("launcher environment with no launcher")
		}
	}
	for _, v := range d.Environment.Volumes {
		if err := ValidateVolume(v); err != nil {
			fail("environment volume: %v", err)
		}
	}

	// Settings.
	if len(d.Settings.Files) > 0 {
		var files []File
		if err := strictJSON(d.Settings.Files, &files); err != nil {
			fail("settings.files: %v", err)
		}
		for _, f := range files {
			homeRelative("settings file", f.Path)
		}
	}
	policyPaths := map[string]bool{}
	for _, p := range d.Settings.Policy {
		asset("policy", p.Asset)
		absolute("policy path", p.Path)
		policyPaths[p.Path] = true
		if !slices.Contains([]string{"managed", "system", "overlay"}, p.Layer) {
			fail("policy %s layer %q is not managed, system or overlay", p.Asset, p.Layer)
		}
	}

	// Local state.
	switch t := d.LocalState.Trust; t.Strategy {
	case "none":
		if t.None == "" {
			fail("trust none needs a reason")
		}
	case "local-state":
		if d.Capture.None != "" {
			fail("local-state trust runs in the capture runtime")
		}
	case "system-layer":
		absolute("trust.layer", t.Layer)
		absolute("trust.lock", t.Lock)
		if !policyPaths[t.Layer] {
			fail("system-layer trust rewrites %s, which no policy asset installs as its baseline", t.Layer)
		}
	default:
		fail("trust strategy %q is not none, local-state or system-layer", t.Strategy)
	}
	switch m := d.LocalState.Memory; m.Strategy {
	case "none":
		if m.None == "" {
			fail("memory none needs a reason")
		}
	case "managed-setting":
		absolute("memory.path", m.Path)
		if !slugPattern.MatchString(m.Name) || m.Key == "" {
			fail("managed-setting memory needs a name and a key")
		}
	case "bind-mount":
		if !slugPattern.MatchString(m.Name) || d.Launch.Home == nil {
			fail("bind-mount memory needs a name and launch.home")
		}
		homeRelative("memory.target", m.Target)
	default:
		fail("memory strategy %q is not none, managed-setting or bind-mount", m.Strategy)
	}
	if d.Launch.None != "" && (d.LocalState.Trust.Strategy != "none" || d.LocalState.Memory.Strategy != "none") {
		fail("local state is prepared by the launcher, and there is none")
	}

	// Credentials.
	var secrets []Secret
	if len(d.Credentials.Secrets) > 0 {
		// Strictly, as the rest of the declaration: a misspelled key would
		// otherwise reach the label and be dropped by every reader of it.
		if err := strictJSON(d.Credentials.Secrets, &secrets); err != nil {
			fail("credentials.secrets: %v", err)
		}
		for _, s := range secrets {
			for _, rule := range s.RejectionScope {
				if err := rule.Validate(); err != nil {
					fail("secret %s: %v", s.Name, err)
				}
			}
		}
	}
	switch s := d.Credentials.Store; s.Strategy {
	case "none":
		if s.None == "" {
			fail("credential store none needs a reason")
		}
	case "file":
		homeRelative("credentials.store.document", s.Document)
	case "native":
		homeRelative("credentials.store.document", s.Document)
		homeRelative("credentials.store.database", s.Database)
		if s.ImportFailure == "" {
			fail("a native store says what a failed import means")
		}
	default:
		fail("credential store %q is not none, file or native", s.Strategy)
	}

	// Hooks.
	if d.Hooks.None == "" {
		if !slugPattern.MatchString(d.Hooks.Provider) {
			fail("hooks.provider %q", d.Hooks.Provider)
		}
		absolute("hooks.path", d.Hooks.Path)
		switch d.Hooks.Transport {
		case "command":
			if !slices.Contains([]string{"claude-code", "codex", "copilot"}, d.Hooks.Format) {
				fail("command hooks format %q is not claude-code, codex or copilot", d.Hooks.Format)
			}
			if len(d.Hooks.Events) == 0 || d.Hooks.Timeout <= 0 {
				fail("command hooks need events and a timeout")
			}
			for _, e := range d.Hooks.Events {
				if !argPattern.MatchString(e) {
					fail("hook event %q", e)
				}
			}
			if d.Hooks.Asset != "" || d.Hooks.HomeFile != "" {
				fail("command hooks are generated, from no asset")
			}
		case "extension":
			asset("hook extension", d.Hooks.Asset)
			homeRelative("hooks.homeFile", d.Hooks.HomeFile)
		case "plugin":
			asset("hook plugin", d.Hooks.Asset)
			named := false
			for _, p := range d.Settings.Policy {
				if raw, err := fs.ReadFile(fsys, path.Join(d.Directory, p.Asset)); err == nil && bytes.Contains(raw, []byte(d.Hooks.Path)) {
					named = true
				}
			}
			if !named {
				fail("plugin hooks at %s are named by no policy asset", d.Hooks.Path)
			}
		default:
			fail("hooks transport %q is not command, extension or plugin", d.Hooks.Transport)
		}
	} else if d.Hooks.Transport != "" {
		fail("hooks are none and declare a transport")
	}

	// Capture, and what it shares with the rest.
	if def := d.SaveBack(); def != nil {
		if err := def.Validate(); err != nil {
			fail("capture: %v", err)
		}
		if s := d.Credentials.Store; s.Strategy != "none" && !slices.Contains(def.CredentialFiles, s.Document) {
			fail("credential document %s is not among capture's credentialFiles", s.Document)
		}
		// A provider spends the same credential its secret is declared
		// for, so both name the same routes.
		for _, p := range def.Providers {
			for _, s := range secrets {
				if s.Name == p.EnvName && !routesEqual(s.Routes, p.Routes) {
					fail("secret %s and provider %s name different routes", s.Name, p.ID)
				}
			}
		}
	} else if d.Coding() {
		fail("capture is none for a harness that installs an agent")
	} else if d.Credentials.Store.Strategy != "none" {
		fail("a credential store with no capture")
	}

	// Every asset is referenced, so none has a lifecycle of its own.
	err := fs.WalkDir(fsys, path.Join(d.Directory, "assets"), func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return err
		}
		rel := strings.TrimPrefix(p, d.Directory+"/")
		if assets[rel] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			fail("asset %s is referenced by nothing in the declaration", rel)
		}
		return nil
	})
	if err != nil {
		fail("assets: %v", err)
	}
	return errors.Join(errs...)
}

func routesEqual(a, b []harnesscapture.Route) bool {
	return reflect.DeepEqual(a, b)
}

// Definition is the built-in harness-config template the server seeds.
func (d *Declaration) Definition() Definition {
	return Definition{ID: d.ID, Name: d.Name, Description: d.Description, Bundle: BundleRef(d.Bundle)}
}
