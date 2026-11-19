// Package harnessbundle stages a built-in harness's bundle from its
// declaration (ADR 26-11-06-596 §1): the manifest label, the feature's
// provision steps, and every script and configuration file they install,
// beside the shared runtime and the declaration's literal assets.
//
// What it writes is build content, never a second source tree: the bundle
// Dockerfile (harness/Dockerfile) builds the staged directory as its
// whole context, and the Taskfile, the release and the development image
// watcher all stage through here. The same inputs stage the same bytes.
package harnessbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

const (
	// RuntimeDir is the shared runtime's source, under the harness tree.
	RuntimeDir = "runtime"
	// Dockerfile is the one bundle Dockerfile, under the harness tree.
	Dockerfile = "Dockerfile"
	// LabelFile is the staged manifest label, which the build passes as
	// harness.MetadataBuildArg.
	LabelFile = "image.json"
	// TargetFile names the Dockerfile stage the staged bundle builds.
	TargetFile = "target"

	// TargetBundle builds the label and the feature; TargetLabel the label
	// alone, for a harness with no feature (the shell), whose bundle has no
	// layer at all.
	TargetBundle = "bundle"
	TargetLabel  = "label"

	// RuntimeInstallDir is where every coding harness installs the shared
	// runtime.
	RuntimeInstallDir = harness.LibexecDir + "/harness-runtime"
	// Launcher is the harness-run convention's command (ADR 0086 §3).
	Launcher = "/usr/local/bin/" + harness.RunCommand
	// LauncherSource is the generated launcher's name in the feature.
	LauncherSource = harness.RunCommand
)

// RuntimeFiles are the shared runtime's files every coding harness carries,
// by their names in harness/runtime. A TypeScript one is compiled to the ES
// module of the same name (.mjs); the rest are copied.
var RuntimeFiles = []string{"main.ts", "runtime.ts", "settings.mjs", "settings-LICENSE.txt", "launch.sh"}

// Bundle is a staged bundle: its declaration and the files the build context
// holds, by slash path, with their modes.
type Bundle struct {
	Declaration *harness.Declaration
	Target      string
	Files       map[string]File
}

// File is one staged file.
type File struct {
	Content []byte
	Mode    fs.FileMode
}

// Generate stages directory's bundle from tree, the harness source tree
// (harness/), in memory.
func Generate(tree fs.FS, directory string) (*Bundle, error) {
	d, err := harness.LoadDeclaration(tree, directory)
	if err != nil {
		return nil, err
	}
	g := &generator{tree: tree, d: d, files: map[string]File{}}
	if err := g.generate(); err != nil {
		return nil, fmt.Errorf("harness %s: %w", directory, err)
	}
	return &Bundle{Declaration: d, Target: Target(d), Files: g.files}, nil
}

// Write makes dir hold exactly the staged bundle: a file no longer generated
// is removed, so it never lingers in a build context, and a file whose content
// and mode are unchanged is left as it is. dir must be absent, empty, or a
// staged bundle already (it holds TargetFile): Write never clears a directory
// it did not stage.
func (b *Bundle) Write(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(dir, TargetFile)); err != nil {
			return fmt.Errorf("%s is not a staged harness bundle; not replacing it", dir)
		}
	}
	// Remove what is no longer staged.
	if len(entries) > 0 {
		var stale []string
		err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			if _, ok := b.Files[filepath.ToSlash(rel)]; !ok {
				stale = append(stale, p)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, p := range stale {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(b.Files)) {
		f := b.Files[name]
		p := filepath.Join(dir, filepath.FromSlash(name))
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == f.Mode {
			if current, err := os.ReadFile(p); err == nil && bytes.Equal(current, f.Content) {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Content, f.Mode); err != nil {
			return err
		}
		// WriteFile's mode is masked by the umask; the bundle's is not.
		if err := os.Chmod(p, f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// FeatureDir is the feature's directory in the staged context.
func (b *Bundle) FeatureDir() string {
	return path.Join(provisionspec.FeaturesDir, b.Declaration.Directory)
}

// Label is the manifest label: the image metadata a bundle publishes in
// harness.ImageLabel.
func Label(d *harness.Declaration) ([]byte, error) {
	type labelHarness struct {
		ID          string          `json:"id"`
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		EgressHosts []string        `json:"egressHosts,omitempty"`
		Files       json.RawMessage `json:"files,omitempty"`
		Secrets     json.RawMessage `json:"secrets,omitempty"`
		SaveBack    any             `json:"saveBack,omitempty"`
	}
	type label struct {
		APIVersion         string            `json:"apiVersion"`
		PlatformAPIVersion string            `json:"platformAPIVersion"`
		Env                map[string]string `json:"env,omitempty"`
		Volumes            []harness.Volume  `json:"volumes,omitempty"`
		Harness            *labelHarness     `json:"harness,omitempty"`
	}
	l := label{APIVersion: harness.ImageAPIVersion, PlatformAPIVersion: d.PlatformAPIVersion, Volumes: d.Environment.Volumes}
	env := map[string]string{}
	for k, v := range d.Environment.Static {
		env[k] = v
	}
	for k, v := range d.Install.Env {
		env[k] = v
	}
	if len(env) > 0 {
		l.Env = env
	}
	if d.Coding() {
		files, err := labelFiles(d)
		if err != nil {
			return nil, err
		}
		h := &labelHarness{ID: d.ID, Name: d.Name, Description: d.Description, EgressHosts: d.Credentials.EgressHosts,
			Files: files, Secrets: d.Credentials.Secrets}
		if def := d.SaveBack(); def != nil {
			h.SaveBack = def
		}
		l.Harness = h
	}
	out, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	// The label must read back as the registry reads every bundle's.
	var check harness.ImageMetadata
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("generated label does not read back: %w", err)
	}
	return append(out, '\n'), nil
}

// labelFiles are the declared seed files, then the one an extension hook
// transport loads its extension from.
func labelFiles(d *harness.Declaration) (json.RawMessage, error) {
	var files []json.RawMessage
	if len(d.Settings.Files) > 0 {
		if err := json.Unmarshal(d.Settings.Files, &files); err != nil {
			return nil, err
		}
	}
	if d.Hooks.Transport == "extension" {
		entry, err := json.Marshal(struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}{d.Hooks.HomeFile, fmt.Sprintf("// Discobox's hooks for %s, installed with the image. Removing this file stops them.\nexport { default } from %q;\n", d.ID, d.Hooks.Path)})
		if err != nil {
			return nil, err
		}
		files = append(files, entry)
	}
	if len(files) == 0 {
		return nil, nil
	}
	return json.Marshal(files)
}

type generator struct {
	tree  fs.FS
	d     *harness.Declaration
	files map[string]File
	steps []step
}

type step struct {
	comment  string
	name     string
	check    string
	run      string
	packages []string
	path     string
	source   string
	mode     string
}

func (g *generator) put(name string, content []byte, mode fs.FileMode) {
	g.files[name] = File{Content: content, Mode: mode}
}

func (g *generator) feature(name string, content []byte, mode fs.FileMode) {
	g.put(path.Join(provisionspec.FeaturesDir, g.d.Directory, name), content, mode)
}

// copyTree copies src (a file or directory of the harness tree) into the
// feature at dst.
func (g *generator) copyTree(src, dst string) ([]string, error) {
	var copied []string
	err := fs.WalkDir(g.tree, src, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(g.tree, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		g.feature(path.Join(dst, rel), data, 0o644)
		copied = append(copied, rel)
		return nil
	})
	return copied, err
}

func (g *generator) file(comment, name, source, dest, mode string) {
	g.steps = append(g.steps, step{comment: comment, name: name, path: dest, source: source, mode: mode})
}

func (g *generator) generate() error {
	d := g.d
	dockerfile, err := fs.ReadFile(g.tree, Dockerfile)
	if err != nil {
		return err
	}
	g.put("Dockerfile", dockerfile, 0o644)
	label, err := Label(d)
	if err != nil {
		return err
	}
	g.put(LabelFile, label, 0o644)
	g.put(TargetFile, []byte(Target(d)+"\n"), 0o644)
	if !d.Coding() {
		return nil
	}

	// The shared runtime, at one place every launcher and capture entry
	// point names.
	for _, source := range RuntimeFiles {
		data, err := fs.ReadFile(g.tree, path.Join(RuntimeDir, source))
		if err != nil {
			return err
		}
		name := source
		if path.Ext(source) == ".ts" {
			name = compiledName(source)
			if data, err = Compile(path.Join(RuntimeDir, source), data); err != nil {
				return err
			}
		}
		g.feature("harness-runtime/"+name, data, 0o644)
		g.file("", "runtime-"+strings.ToLower(strings.TrimSuffix(name, path.Ext(name))), "harness-runtime/"+name, RuntimeInstallDir+"/"+name, "")
	}
	g.steps[0].comment = "The shared harness runtime: the launcher's library and the capture\nruntime. Every coding harness carries its own copy; the platform has no\nJavaScript runtime."

	// Install.
	if pkgs := d.Install.OSPackages; pkgs != nil && len(pkgs.Debian) > 0 {
		g.steps = append(g.steps, step{comment: "Distribution packages the CLI looks for on PATH.", name: "os-packages", packages: pkgs.Debian})
	}
	var install strings.Builder
	install.WriteString(generatedHeader(d, "#"))
	install.WriteString("set -eu\n")
	for _, name := range slices.Sorted(mapKeys(d.Install.Env)) {
		fmt.Fprintf(&install, "export %s=%s\n", name, shellQuote(d.Install.Env[name]))
	}
	fmt.Fprintf(&install, "exec %s/npm-global install %s\n", harness.LibexecDir, d.Install.Package)
	g.feature("install-cli.sh", []byte(install.String()), 0o755)
	g.feature("check-cli.sh", fmt.Appendf(nil, "%sexec %s/npm-global check %s\n", generatedHeader(d, "#"), harness.LibexecDir, d.Install.Package), 0o755)
	g.steps = append(g.steps, step{
		comment: "The CLI, from npm, at its latest release when this step is applied: into\nDiscobox's npm prefix, its commands linked into /usr/local/bin. The check\ninstalls it again when /usr/local/bin's command is not this one.",
		name:    "cli", check: "check-cli.sh", run: "install-cli.sh",
	})

	// Hooks.
	switch d.Hooks.Transport {
	case "command":
		config, err := commandHooks(d)
		if err != nil {
			return err
		}
		g.feature("hooks.json", config, 0o644)
		g.file("Command hooks publishing every declared lifecycle event (ADR 0146).", "hooks", "hooks.json", d.Hooks.Path, "")
	case "extension":
		if _, err := g.copyTree(path.Join(d.Directory, d.Hooks.Asset), d.Hooks.Asset); err != nil {
			return err
		}
		g.file("The hook extension, loaded from the home file the manifest delivers.", "hooks", d.Hooks.Asset, d.Hooks.Path, "")
	case "plugin":
		copied, err := g.copyTree(path.Join(d.Directory, d.Hooks.Asset), d.Hooks.Asset)
		if err != nil {
			return err
		}
		sort.Strings(copied)
		for i, rel := range copied {
			comment := ""
			if i == 0 {
				comment = "The hook plugin, a directory the policy layer names by its path."
			}
			g.file(comment, "hooks-"+strings.TrimSuffix(path.Base(rel), path.Ext(rel)), path.Join(d.Hooks.Asset, rel), path.Join(d.Hooks.Path, rel), "")
		}
	}

	// Policy.
	for _, p := range d.Settings.Policy {
		if _, err := g.copyTree(path.Join(d.Directory, p.Asset), p.Asset); err != nil {
			return err
		}
		g.file(fmt.Sprintf("Policy, the vendor's %s layer.", p.Layer), "policy-"+stepName(p.Asset), p.Asset, p.Path, "")
		if t := d.LocalState.Trust; t.Strategy == "system-layer" && t.Layer == p.Path {
			g.file("The system layer's baseline, which folder trust rewrites the layer from.", "trust-baseline", p.Asset, TrustBaseline(d, p), "")
		}
	}

	// Login-shell environment.
	if d.Environment.Profile != "" {
		var profile strings.Builder
		profile.WriteString("# shellcheck shell=sh # Sourced by /etc/profile, never run.\n")
		profile.WriteString(strings.TrimPrefix(generatedHeader(d, "#"), "#!/bin/sh\n"))
		for _, a := range d.Environment.Aliases {
			fmt.Fprintf(&profile, "if [ -n \"${%[2]s:-}\" ] && [ -z \"${%[1]s:-}\" ]; then\n\texport %[1]s=\"$%[2]s\"\nfi\n", a.Name, a.From)
		}
		g.feature("profile.sh", []byte(profile.String()), 0o644)
		g.file("The environment aliases, for every login shell.", "profile-env", "profile.sh", d.Environment.Profile, "")
	}

	// The version store's configuration (ADR 0114 §7).
	g.feature("agent.conf", fmt.Appendf(nil, "# The agent this harness installs, for the pool-cached version store\n# (ADR 0114 §7), generated from harness/%s/harness.json.\nAGENT_PACKAGE=%s\nAGENT_BIN=%s\n", d.Directory, d.Install.Package, d.Install.Executable), 0o644)
	g.file("The agent this harness installs, for the pool-cached version store.", "agent-conf", "agent.conf", harness.LibexecDir+"/agent.conf", "")

	// The launcher, over core's no-op, and its drift check.
	launcher, err := launcherScript(d)
	if err != nil {
		return err
	}
	g.feature(LauncherSource, launcher, 0o755)
	g.feature("check-launcher.sh", fmt.Appendf(nil, "%sexec cmp -s %s %s\n", generatedHeader(d, "#"), LauncherSource, Launcher), 0o755)
	g.steps = append(g.steps, step{
		comment: "The harness-run convention (ADR 0086 §3), over core's no-op. The check\nputs it back when core, applied again, laid its own there.",
		name:    "harness-run", check: "check-launcher.sh", path: Launcher, source: LauncherSource, mode: "0755",
	})

	// Capture and the runtime's view of this harness.
	capture := fmt.Sprintf("%sexec node \"$(dirname \"$0\")/harness-runtime/main.mjs\" %s \"$(dirname \"$0\")/%s-image.json\" \"$@\"\n", generatedHeader(d, "#"), d.ID, d.ID)
	g.feature("capture.sh", []byte(capture), 0o755)
	g.file("The capture entry point the manifest's save-back names.", "capture", "capture.sh", d.CaptureCommand(), "0755")
	g.feature("image.json", label, 0o644)
	g.file("", "capture-metadata", "image.json", harness.LibexecDir+"/"+d.ID+"-image.json", "")
	declaration, err := fs.ReadFile(g.tree, path.Join(d.Directory, harness.DeclarationFile))
	if err != nil {
		return err
	}
	g.feature(harness.DeclarationFile, declaration, 0o644)
	g.file("", "capture-declaration", harness.DeclarationFile, harness.LibexecDir+"/"+d.ID+"-harness.json", "")
	source := path.Join(d.Directory, harness.AdapterFile)
	adapter, err := fs.ReadFile(g.tree, source)
	if err != nil {
		return err
	}
	if adapter, err = Compile(source, adapter); err != nil {
		return err
	}
	compiled := compiledName(harness.AdapterFile)
	g.feature(compiled, adapter, 0o644)
	g.file("", "capture-adapter", compiled, harness.LibexecDir+"/"+d.ID+"-adapter.mjs", "")

	provision, err := g.provision()
	if err != nil {
		return err
	}
	g.feature(provisionspec.FileName, provision, 0o644)
	return nil
}

// Target is the Dockerfile stage d's bundle is built from: the label alone
// for a harness with no feature.
func Target(d *harness.Declaration) string {
	if d.Coding() {
		return TargetBundle
	}
	return TargetLabel
}

// TrustBaseline is where a system-layer trust strategy's baseline is
// installed beside the layer it rewrites.
func TrustBaseline(d *harness.Declaration, p harness.Policy) string {
	return harness.LibexecDir + "/" + d.ID + "-system-config" + path.Ext(p.Asset)
}

func stepName(asset string) string {
	base := path.Base(asset)
	return strings.TrimSuffix(base, path.Ext(base))
}

func (g *generator) provision() ([]byte, error) {
	d := g.d
	var b strings.Builder
	fmt.Fprintf(&b, "# The %s harness's feature, generated from harness/%s/harness.json by\n# discobox-harness-bundle (ADR 26-11-06-596). Change the declaration, not\n# this file.\napiVersion: %s\nrequires: [%s]\nsteps:\n",
		d.Name, d.Directory, provisionspec.APIVersion, strings.Join(slices.Sorted(slices.Values(d.Install.Requires)), ", "))
	seen := map[string]bool{}
	for _, s := range g.steps {
		if seen[s.name] {
			return nil, fmt.Errorf("two provision steps named %s", s.name)
		}
		seen[s.name] = true
		if s.comment != "" {
			for _, line := range strings.Split(s.comment, "\n") {
				fmt.Fprintf(&b, "  # %s\n", line)
			}
		}
		fmt.Fprintf(&b, "  - name: %s\n", s.name)
		if s.check != "" {
			fmt.Fprintf(&b, "    check: %s\n", s.check)
		}
		switch {
		case s.run != "":
			fmt.Fprintf(&b, "    run: %s\n", s.run)
		case len(s.packages) > 0:
			fmt.Fprintf(&b, "    packages:\n      debian: [%s]\n", strings.Join(s.packages, ", "))
		default:
			mode := ""
			if s.mode != "" {
				mode = fmt.Sprintf(", mode: %q", s.mode)
			}
			fmt.Fprintf(&b, "    file: {path: %s, source: %s%s}\n", s.path, s.source, mode)
		}
	}
	out := []byte(b.String())
	spec, err := provisionspec.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("generated provision.yaml: %w", err)
	}
	if !slices.Contains(spec.Requires, provisionspec.Core) {
		return nil, errors.New("generated provision.yaml does not require core")
	}
	return out, nil
}

func generatedHeader(d *harness.Declaration, comment string) string {
	return fmt.Sprintf("#!/bin/sh\n%[1]s Generated from harness/%[2]s/harness.json by discobox-harness-bundle\n%[1]s (ADR 26-11-06-596). Change the declaration, not this file.\n", comment, d.Directory)
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// launcherScript is discobox-harness-run: the declared launch, as calls into
// the shared launcher library.
func launcherScript(d *harness.Declaration) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# discobox-harness-run for %s, generated from harness/%s/harness.json by\n# discobox-harness-bundle (ADR 26-11-06-596). Change the declaration, not\n# this file; what each call does is harness/runtime/launch.sh's.\nset -eu\n. %s\n",
		d.Name, d.Directory, shellQuote(RuntimeInstallDir+"/launch.sh"))
	for _, name := range slices.Sorted(mapKeys(d.Environment.Defaults)) {
		fmt.Fprintf(&b, "discobox_harness_env_default %s %s\n", name, shellQuote(d.Environment.Defaults[name]))
	}
	for _, a := range d.Environment.Aliases {
		fmt.Fprintf(&b, "discobox_harness_env_alias %s %s\n", a.Name, a.From)
	}
	home := func() string {
		h := d.Launch.Home
		return fmt.Sprintf("%s %s", shellQuote(h.Env), shellQuote(h.Default))
	}
	switch m := d.LocalState.Memory; m.Strategy {
	case "managed-setting":
		fmt.Fprintf(&b, "discobox_harness_memory_managed_setting %s %s %s %s\n", shellQuote(d.Name), m.Name, shellQuote(m.Path), shellQuote(m.Key))
	case "bind-mount":
		fmt.Fprintf(&b, "discobox_harness_memory_bind_mount %s %s %s %s\n", shellQuote(d.Name), m.Name, home(), shellQuote(m.Target))
	}
	if t := d.LocalState.Trust; t.Strategy == "system-layer" {
		fmt.Fprintf(&b, "discobox_harness_trust_system_layer %s %s\n", shellQuote(t.Lock), shellQuote(d.CaptureCommand()))
	}
	if NeedsPreparation(d) {
		fmt.Fprintf(&b, "discobox_harness_prepare %s\n", shellQuote(d.CaptureCommand()))
	}
	fmt.Fprintf(&b, "discobox_harness_run --name %s --executable %s", shellQuote(d.Name), d.Install.Executable)
	if len(d.Launch.Args) > 0 {
		fmt.Fprintf(&b, " --args %s", shellQuote(strings.Join(d.Launch.Args, " ")))
	}
	fmt.Fprintf(&b, " --resume %s --prompt %s", shellQuote(strings.Join(d.Launch.Resume, " ")), d.Launch.Prompt.Form)
	if d.Launch.Prompt.Option != "" {
		fmt.Fprintf(&b, " --prompt-option %s", shellQuote(d.Launch.Prompt.Option))
	}
	if d.Launch.Prompt.LeadingAt != "" {
		fmt.Fprintf(&b, " --leading-at %s", d.Launch.Prompt.LeadingAt)
	}
	if r := d.Launch.Retry; r != nil {
		delays := make([]string, len(r.Delays))
		for i, delay := range r.Delays {
			delays[i] = strconv.Itoa(delay)
		}
		fmt.Fprintf(&b, " --retry %d %s %s %s", r.Status, shellQuote(strings.Join(delays, " ")), home(), shellQuote(r.Sessions))
	}
	b.WriteString(" -- \"$@\"\n")
	return []byte(b.String()), nil
}

// NeedsPreparation reports whether the launcher runs the capture runtime's
// prepare under the capture lock before the CLI: to prepare local trust, to
// import a native credential store, or to derive environment from settings.
func NeedsPreparation(d *harness.Declaration) bool {
	return d.LocalState.Trust.Strategy == "local-state" || d.Credentials.Store.Strategy == "native" || d.Environment.FromSettings != ""
}

// commandHooks is a command transport's configuration, in its vendor's
// format, publishing each declared event.
func commandHooks(d *harness.Declaration) ([]byte, error) {
	h := d.Hooks
	var b bytes.Buffer
	command := func(event string) string {
		return fmt.Sprintf("discobox-hook-publish --provider %s --event %s", h.Provider, event)
	}
	b.WriteString("{\n")
	switch h.Format {
	case "codex":
		fmt.Fprintf(&b, "  \"description\": %s,\n", jsonString(fmt.Sprintf("Publish %s lifecycle events to the Discobox sandbox agent.", d.Name)))
		fallthrough
	case "claude-code":
		b.WriteString("  \"hooks\": {\n")
		for i, e := range h.Events {
			fmt.Fprintf(&b, "    %s: [{ \"hooks\": [{ \"type\": \"command\", \"command\": %s, \"timeout\": %d }] }]%s\n", jsonString(e), jsonString(command(e)), h.Timeout, comma(i, len(h.Events)))
		}
	case "copilot":
		b.WriteString("  \"version\": 1,\n  \"hooks\": {\n")
		for i, e := range h.Events {
			fmt.Fprintf(&b, "    %s: [{ \"type\": \"command\", \"bash\": %s, \"timeoutSec\": %d }]%s\n", jsonString(e), jsonString(command(e)), h.Timeout, comma(i, len(h.Events)))
		}
	default:
		return nil, fmt.Errorf("hooks format %q", h.Format)
	}
	b.WriteString("  }\n}\n")
	if !json.Valid(b.Bytes()) {
		return nil, errors.New("generated hooks configuration is not JSON")
	}
	return b.Bytes(), nil
}

func jsonString(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err) // A string always marshals.
	}
	return string(out)
}

func comma(i, n int) string {
	if i == n-1 {
		return ""
	}
	return ","
}

// StageDir is where bundles are staged in a checkout, by directory. It is
// under build/, which no image context includes.
const StageDir = "build/harness-bundles"

// Directory resolves a harness named by its id or its directory to its
// directory in the inventory.
func Directory(tree fs.FS, name string) (string, error) {
	builtins, err := harness.LoadBuiltins(tree)
	if err != nil {
		return "", err
	}
	for _, d := range builtins {
		if d.ID == name || d.Directory == name {
			return d.Directory, nil
		}
	}
	return "", fmt.Errorf("unknown harness %q", name)
}

// Install writes every file the feature's file steps install under root, at
// root plus the step's path, as a box's provisioning would: for a test that
// runs a harness's installed commands without a box. It runs nothing.
func (b *Bundle) Install(root string) error {
	spec, err := provisionspec.Parse(b.Files[b.FeatureDir()+"/"+provisionspec.FileName].Content)
	if err != nil {
		return err
	}
	for _, step := range spec.Steps {
		if step.File == nil {
			continue
		}
		f, ok := b.Files[b.FeatureDir()+"/"+step.File.Source]
		if !ok {
			return fmt.Errorf("step %s installs %s, which is not staged", step.Name, step.File.Source)
		}
		target := filepath.Join(root, filepath.FromSlash(step.File.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if step.File.Mode != "" {
			parsed, err := strconv.ParseUint(step.File.Mode, 8, 32)
			if err != nil {
				return err
			}
			mode = fs.FileMode(parsed)
		}
		if err := os.WriteFile(target, f.Content, mode); err != nil {
			return err
		}
	}
	return nil
}
