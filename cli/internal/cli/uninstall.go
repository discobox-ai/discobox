package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/internal/filelock"
)

// `discobox admin uninstall` takes back everything Discobox keeps on this machine
// apart from the discobox command itself: the CLI's state, the servers and
// images it downloaded, the local server's data, configuration, cache, state
// and runtime files, the Includes it put in ~/.ssh/config, and what the
// install scripts leave beside the command.
//
// It looks before it asks and asks before it touches anything. The plan is
// what gets deleted — nothing is removed that was not listed — so every
// question of what is Discobox's is settled while building it, and the answer
// is read from stdin rather than taken from a flag: a deletion this large is
// something somebody says yes to having seen.

// uninstallServerStopTimeout is how long a local server gets to stop before
// the uninstall gives up on it. Stopping drains its providers — containers and
// VMs — which is the reason to ask it rather than delete from under it.
const uninstallServerStopTimeout = 2 * serverStopGrace

const (
	// serverSingletonLockName is the lock a server holds in its data directory
	// for as long as it runs (server/internal/server/singleton.go).
	serverSingletonLockName = "server.lock"
	// serverStartupLockName is the launch lock an autolaunch takes in the
	// temporary directory when the endpoint is not a socket (endpoint's
	// LaunchOptions.lockPath) — which is every launch on Windows.
	serverStartupLockName = "discobox-server-startup.lock"
)

// uninstallLocation is one place Discobox keeps something, and what it keeps
// there, before anything has been checked about it.
type uninstallLocation struct {
	path string
	what string
	// env is the variable that named path, when one did. Such a path is
	// wherever somebody pointed it, not a directory this program made.
	env string
}

// uninstallEntry is one path the uninstall removes: the outermost of the
// locations that were found, with everything nested inside it folded in.
type uninstallEntry struct {
	path string
	what []string
	size int64
}

// uninstallKept is a location that was found and will not be removed, and why.
type uninstallKept struct {
	path   string
	what   string
	reason string
}

// uninstallServer is a local server that answered, and has to stop before its
// files go.
type uninstallServer struct {
	endpoint string
	baseURL  string
	client   *http.Client
}

// uninstallSSHConfig is the edit to this machine's ssh_config: the Includes
// of discobox configs that are removed, and the config without them.
type uninstallSSHConfig struct {
	target  sshTarget
	removed []string
	body    string
}

type uninstallPlan struct {
	servers []uninstallServer
	entries []uninstallEntry
	kept    []uninstallKept
	ssh     uninstallSSHConfig
}

func (p uninstallPlan) empty() bool {
	return len(p.servers) == 0 && len(p.entries) == 0 && len(p.ssh.removed) == 0
}

func (a *App) newUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Delete Discobox's data, downloads and configuration from this machine",
		Long: `Delete everything Discobox keeps on this machine except the discobox command.

That is the CLI's state (this machine's identity and SSH keys), the servers and
images it downloaded, the local server's database, pool disks, configuration,
cache and logs, the Include lines it added to ~/.ssh/config, and what the
install scripts leave beside the command (the binary an upgrade replaced, an
interrupted install's copy). A local server that is running is asked to stop
first.

Everything that would be deleted is listed first, and nothing is deleted until
you answer yes. To remove the discobox command as well, delete it the way it was
installed (for example, brew uninstall discobox).

What this cannot see is left: directories a server.yaml or a
.discobox-server.env relocates, disk directories set in a provider's own
configuration, the file DISCOBOX_CONFIG_FILE names, and the containers,
volumes and images a Docker provider created. A directory an environment
variable names is deleted only when its name says it is Discobox's.

Close editors and discobox windows first: anything that starts a server again
while this runs recreates what it deletes.`,
		Args: cobra.NoArgs,
		// The root's hook reads the registered servers and validates the
		// environment. A machine whose configuration no longer parses is one
		// somebody may well be uninstalling, so it must not be what stops them.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.errOut = cmd.ErrOrStderr()
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.uninstall(cmd)
		},
	}
}

func (a *App) uninstall(cmd *cobra.Command) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	plan := a.planUninstall(ctx)
	writeUninstallPlan(out, plan)
	if plan.empty() {
		return nil
	}

	fmt.Fprint(out, "\nDelete everything listed above? [y/N] ")
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read the answer: %w", err)
	}
	if answer == "" && errors.Is(err, io.EOF) {
		fmt.Fprintln(out)
		return errors.New("no answer on stdin: nothing was deleted")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
	default:
		fmt.Fprintln(out, "Nothing was deleted.")
		return nil
	}

	// Every server stops before anything is deleted, and one that will not
	// stop ends the uninstall there: removing a database and VM disks from
	// under a running server leaves it serving from files that are gone.
	for _, server := range plan.servers {
		if err := stopUninstallServer(ctx, server); err != nil {
			return fmt.Errorf("stop the server at %s: %w; nothing was deleted", server.endpoint, err)
		}
		fmt.Fprintf(out, "Stopped the server at %s\n", server.endpoint)
	}
	// A server stops answering before it has stopped: it closes its listeners
	// first, then drains its providers and closes its database, and releases
	// the data directory's lock last. That lock is what says it is gone, and
	// it is waited for even when nothing answered — a server can be mid-start
	// or mid-stop without answering either.
	if err := waitForServerLock(ctx, serverDataDir(), uninstallServerStopTimeout); err != nil {
		return fmt.Errorf("%w; nothing was deleted", err)
	}

	var errs []error
	if len(plan.ssh.removed) > 0 {
		if err := plan.ssh.target.writeUserConfig(ctx, plan.ssh.body); err != nil {
			errs = append(errs, err)
		} else {
			fmt.Fprintf(out, "Removed %s from %s\n", plural(len(plan.ssh.removed), "Include line", "Include lines"), plan.ssh.target.userConfig.local)
		}
	}
	for _, entry := range plan.entries {
		if err := os.RemoveAll(entry.path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", entry.path, err))
			continue
		}
		fmt.Fprintf(out, "Removed %s\n", entry.path)
	}
	// Nothing can stop an editor's ProxyCommand or a launcher left open from
	// starting a server again while the deletes run, and one that did has put
	// back some of what was just removed. Saying so beats reporting a clean
	// machine that is not one.
	if restarted := a.answeringLocalServers(ctx); len(restarted) > 0 {
		errs = append(errs, fmt.Errorf("a server started again at %s while this ran and may have recreated what was deleted; "+
			"close whatever started it and run this again", restarted[0].endpoint))
	}
	return errors.Join(errs...)
}

// planUninstall finds what is there to remove. It changes nothing.
func (a *App) planUninstall(ctx context.Context) uninstallPlan {
	var plan uninstallPlan
	plan.servers = a.answeringLocalServers(ctx)
	plan.entries, plan.kept = uninstallEntries(uninstallLocations(), runningExecutables(), protectedDirs())
	// No home directory is no ssh_config to have written an Include into.
	if target, err := localSSHTarget(); err == nil {
		ssh, err := uninstallSSHIncludes(target, plan.entries)
		if err != nil {
			plan.kept = append(plan.kept, uninstallKept{path: target.userConfig.local, what: "discobox Include lines", reason: err.Error()})
		}
		plan.ssh = ssh
	}
	return plan
}

// uninstallLocations is every place Discobox keeps something on this machine,
// resolved the way the program that writes there resolves it. They overlap —
// on Linux the CLI's state root and the server's state directory are one
// directory — and uninstallEntries folds the overlaps together.
//
// The server's directories follow its defaults and the environment variables
// that move them (server/internal/config): the environment is this process's
// too, while a server.yaml is the server's to read.
func uninstallLocations() []uninstallLocation {
	locations := []uninstallLocation{
		{path: discoboxStateDir()},
		{path: cliStateDir(), what: "CLI state: this machine's identity, SSH keys and configs, history"},
		{path: stagedServerRoot(), what: "downloaded servers"},
		{path: stagedImagesRoot(), what: "downloaded images", env: envNamed(ImageCacheEnv)},
		{path: serverDataDir(), what: "server data: database, keys", env: envNamed("DISCOBOX_DATA_DIR")},
		// VM providers keep their pool disks and guest images here whatever
		// the server's data directory is.
		{path: baseDir(xdg.DataHome), what: "pool disks and VM images"},
		// server.yaml, servers.json and host-id are found here whatever
		// DISCOBOX_CONFIG_DIR says; that variable moves the server's other
		// configuration only.
		{path: baseDir(xdg.ConfigHome), what: "configuration: server.yaml, registered servers, host ID"},
		{path: serverDir("DISCOBOX_CONFIG_DIR", xdg.ConfigHome), what: "server configuration", env: envNamed("DISCOBOX_CONFIG_DIR")},
		{path: serverDir("DISCOBOX_CACHE_DIR", xdg.CacheHome), what: "server cache", env: envNamed("DISCOBOX_CACHE_DIR")},
		{path: serverDir("DISCOBOX_STATE_DIR", xdg.StateHome), what: "server state and logs", env: envNamed("DISCOBOX_STATE_DIR")},
		{path: discoboxRuntimeDir(), what: "server socket and runtime files"},
	}
	if dir, err := toolConfigDir(); err == nil {
		locations = append(locations, uninstallLocation{path: filepath.Dir(dir), what: "tool configuration"})
	}
	locations = append(locations, installerLeftovers()...)
	return append(locations, tempDirLeftovers()...)
}

// serverDataDir is the local server's data directory, which holds its
// singleton lock.
func serverDataDir() string {
	return serverDir("DISCOBOX_DATA_DIR", xdg.DataHome)
}

// envNamed is name when that variable is set, so a location says what put it
// there.
func envNamed(name string) string {
	if strings.TrimSpace(os.Getenv(name)) != "" {
		return name
	}
	return ""
}

// baseDir is discobox under one of the platform's base directories.
func baseDir(base string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(base, "discobox")
}

// installerLeftovers is what install.sh and install.ps1 leave behind besides
// the command itself, in the directories they install into: the ones this
// command runs from, DISCOBOX_INSTALL_DIR, and both scripts' defaults for a
// user (/usr/local/bin, install.sh's default as root, is covered when it is
// where this command runs from).
//
//   - install.ps1 on Windows renames the command it replaces to discobox.exe.old
//     (discobox.exe.<guid>.old when that is taken), since a running executable
//     can be renamed and not overwritten. Nothing removes those afterwards.
//   - Both copy the download in beside the destination before renaming it into
//     place — install.sh as .discobox.<pid>, install.ps1 as .discobox.new or
//     .discobox.exe.new — and a run stopped in between leaves that copy.
//
// install.ps1's temporary download directory is tempDirLeftovers'.
//
// The PATH entry install.ps1 adds is left: it names the directory the command
// stays in.
func installerLeftovers() []uninstallLocation {
	var dirs []string
	for _, executable := range runningExecutables() {
		dirs = append(dirs, filepath.Dir(executable))
	}
	if dir := strings.TrimSpace(os.Getenv("DISCOBOX_INSTALL_DIR")); dir != "" {
		dirs = append(dirs, dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		dirs = append(dirs, filepath.Join(local, "Programs", "Discobox"))
	}

	var locations []uninstallLocation
	seen := map[string]bool{}
	add := func(path, what string) {
		if !seen[path] {
			seen[path] = true
			locations = append(locations, uninstallLocation{path: path, what: what})
		}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if what := installerLeftoverName(entry.Name()); what != "" {
				add(filepath.Join(dir, entry.Name()), what)
			}
		}
	}
	return locations
}

// tempDirLeftovers is what Discobox leaves in the temporary directory: the
// download directory of an install.ps1 run that was killed before it cleaned
// up, and the server startup lock.
func tempDirLeftovers() []uninstallLocation {
	var locations []uninstallLocation
	if entries, err := os.ReadDir(os.TempDir()); err == nil {
		for _, entry := range entries {
			var what string
			switch {
			case entry.IsDir() && strings.HasPrefix(entry.Name(), "discobox-install-"):
				what = "an interrupted install's download"
			case entry.Name() == serverStartupLockName:
				what = "server startup lock"
			default:
				continue
			}
			// A temporary directory can be shared with other users, and
			// what they left there is theirs.
			if info, err := entry.Info(); err == nil && ownedByThisUser(info) {
				locations = append(locations, uninstallLocation{path: filepath.Join(os.TempDir(), entry.Name()), what: what})
			}
		}
	}
	return locations
}

// installerLeftoverName says what an install left a file of this name for, or
// nothing when it is not one of theirs.
func installerLeftoverName(name string) string {
	switch {
	case strings.EqualFold(name, "discobox.exe.old"),
		strings.HasPrefix(strings.ToLower(name), "discobox.exe.") && strings.HasSuffix(strings.ToLower(name), ".old"):
		return "the discobox command an upgrade replaced"
	case name == ".discobox.new", strings.EqualFold(name, ".discobox.exe.new"):
		return "an interrupted install's copy of the command"
	}
	if pid, ok := strings.CutPrefix(name, ".discobox."); ok && pid != "" && strings.Trim(pid, "0123456789") == "" {
		return "an interrupted install's copy of the command"
	}
	return ""
}

// serverDir is one of the server's directories: what the environment names,
// or discobox under the platform's base directory.
func serverDir(env, base string) string {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return value
	}
	return baseDir(base)
}

// uninstallEntries turns locations into what is removed: the ones that exist,
// with a location inside another folded into the outer one, and without any
// that would take something that is not Discobox's with it.
//
// A location holding a protected directory is kept rather than trimmed to its
// Discobox parts. It is there because something named it — DISCOBOX_DATA_DIR
// set to a home directory, say — and guessing which part of it is ours is not
// a guess to make with RemoveAll.
func uninstallEntries(locations []uninstallLocation, executables, protected []string) ([]uninstallEntry, []uninstallKept) {
	var present []uninstallLocation
	for _, location := range locations {
		if location.path == "" {
			continue
		}
		abs, err := filepath.Abs(location.path)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(abs); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		present = append(present, uninstallLocation{path: abs, what: location.what, env: location.env})
	}
	// Outermost first, so a location is folded into one that holds it: a
	// directory's path is always shorter than anything inside it.
	sort.SliceStable(present, func(i, j int) bool { return len(present[i].path) < len(present[j].path) })

	var entries []uninstallEntry
	var kept []uninstallKept
	for _, location := range present {
		if reason := uninstallKeepReason(location, executables, protected); reason != "" {
			if !slices.ContainsFunc(kept, func(k uninstallKept) bool { return samePathOnDisk(k.path, location.path) }) {
				kept = append(kept, uninstallKept{path: location.path, what: location.what, reason: reason})
			}
			continue
		}
		holder := slices.IndexFunc(entries, func(e uninstallEntry) bool { return pathWithin(e.path, location.path) })
		if holder < 0 {
			entries = append(entries, uninstallEntry{path: location.path})
			holder = len(entries) - 1
		}
		if location.what != "" && !slices.Contains(entries[holder].what, location.what) {
			entries[holder].what = append(entries[holder].what, location.what)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for i := range entries {
		entries[i].size = diskUsage(entries[i].path)
	}
	return entries, kept
}

// uninstallKeepReason says why a location must not be removed, or nothing when
// it may be.
//
// A variable can name anything — DISCOBOX_STATE_DIR=. in a project's .envrc is
// the checkout — so a directory one names is taken only when its own name says
// it is Discobox's. Everything else here is discobox under a base directory,
// which is Discobox's by construction.
func uninstallKeepReason(location uninstallLocation, executables, protected []string) string {
	path := location.path
	if location.env != "" && !strings.Contains(strings.ToLower(filepath.Base(path)), "discobox") {
		return location.env + " names it, and its name does not say it is Discobox's; delete it yourself if it is"
	}
	for _, executable := range executables {
		if pathWithin(path, executable) {
			return "it holds this discobox command"
		}
	}
	for _, dir := range protected {
		if samePathOnDisk(path, dir) {
			return "it is " + dir + ", not a directory of Discobox's own"
		}
		if pathWithin(path, dir) {
			return "it holds " + dir
		}
	}
	return ""
}

// runningExecutables is this executable as it was started and as its symlinks
// resolve. The discobox command is the one thing an uninstall leaves.
func runningExecutables() []string {
	executable, err := os.Executable()
	if err != nil {
		return nil
	}
	paths := []string{executable}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil && resolved != executable {
		paths = append(paths, resolved)
	}
	return paths
}

// protectedDirs are the directories Discobox's own directories sit inside. A
// location that is one of them, or holds one, was pointed somewhere that is
// not Discobox's, and deleting it would take everything else there along.
func protectedDirs() []string {
	var dirs []string
	add := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dirs = append(dirs, abs)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		add(dir)
	}
	if dir, err := os.UserCacheDir(); err == nil {
		add(dir)
	}
	add(xdg.DataHome)
	add(xdg.ConfigHome)
	add(xdg.StateHome)
	add(xdg.CacheHome)
	add(os.Getenv("XDG_RUNTIME_DIR"))
	add(os.TempDir())
	if dir, err := os.Getwd(); err == nil {
		add(dir)
	}
	return dirs
}

// pathWithin reports whether p is dir or inside it.
func pathWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// samePathOnDisk reports whether a and b name the same path, in the case
// sensitivity of this platform's filepath.Rel.
func samePathOnDisk(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

// diskUsage is the size of everything under path, not following symlinks. It
// is only ever shown, so what cannot be read is simply not counted.
func diskUsage(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// answeringLocalServers is every local server that answers on the endpoints
// this machine's server can be at: the default one, and --server when it names
// a local one. A server reached over the network is not this machine's to stop,
// and its files are not here.
func (a *App) answeringLocalServers(ctx context.Context) []uninstallServer {
	candidates := []string{endpoint.DefaultEndpoint()}
	if a.serverURL != "" {
		candidates = append(candidates, a.serverURL)
	}
	var servers []uninstallServer
	seen := map[string]bool{}
	for _, raw := range candidates {
		parsed, err := endpoint.Parse(raw)
		if err != nil || !parsed.AutoLaunchable() {
			continue
		}
		key := parsed.Scheme + "://" + parsed.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		baseURL, client, err := endpoint.HTTPClient(parsed, nil)
		if err != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err = endpoint.ProbeHealth(probeCtx, baseURL, client)
		cancel()
		if err != nil {
			continue
		}
		servers = append(servers, uninstallServer{endpoint: raw, baseURL: baseURL, client: client})
	}
	return servers
}

// waitForServerLock waits until no server holds the singleton lock in dataDir.
// A data directory that is not there has no server in it.
func waitForServerLock(ctx context.Context, dataDir string, timeout time.Duration) error {
	if dataDir == "" {
		return nil
	}
	path := filepath.Join(dataDir, serverSingletonLockName)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			return lock.Release()
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return fmt.Errorf("check whether a server holds %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			holder := "a server"
			if pid, ok := filelock.HolderPID(path); ok {
				holder = fmt.Sprintf("a server (pid %d)", pid)
			}
			return fmt.Errorf("%s still holds %s after %s", holder, path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func stopUninstallServer(ctx context.Context, server uninstallServer) error {
	resp, err := requestServerShutdown(ctx, server.baseURL, server.client)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return waitForServerShutdown(ctx, server.baseURL, server.client, uninstallServerStopTimeout)
}

// uninstallSSHIncludes finds the Include lines this CLI added to this
// machine's ssh_config that name a file the uninstall removes, or one already
// gone. An Include of a config that is still there — under a location that is
// kept — still works, and stays.
//
// Only this machine's ssh. On WSL the Windows side has an ssh_config of its
// own, and its files sit beside a Windows install's state, which an uninstall
// from inside a distribution has no business deciding about.
func uninstallSSHIncludes(target sshTarget, entries []uninstallEntry) (uninstallSSHConfig, error) {
	edit := uninstallSSHConfig{target: target}
	existing, err := os.ReadFile(target.userConfig.local)
	if errors.Is(err, fs.ErrNotExist) {
		return edit, nil
	}
	if err != nil {
		return edit, fmt.Errorf("could not read it: %w", err)
	}
	var kept []string
	for _, line := range strings.Split(string(existing), "\n") {
		field, ok := target.includeField(line)
		path := target.clean(unescapeSSHPercent(field))
		if ok && target.isManagedConfigPath(path) && uninstallRemoves(entries, path) {
			edit.removed = append(edit.removed, strings.TrimSpace(line))
			continue
		}
		kept = append(kept, line)
	}
	if len(edit.removed) > 0 {
		// The Include was written at the top with a blank line after it, and
		// the blank would otherwise be what the file now starts with.
		edit.body = strings.TrimLeft(collapseBlankRuns(strings.Join(kept, "\n")), "\n")
	}
	return edit, nil
}

// uninstallRemoves reports whether path will be gone once the uninstall has
// run: it is under an entry, or it is not there now.
func uninstallRemoves(entries []uninstallEntry, path string) bool {
	if slices.ContainsFunc(entries, func(e uninstallEntry) bool { return pathWithin(e.path, path) }) {
		return true
	}
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func writeUninstallPlan(out io.Writer, plan uninstallPlan) {
	if plan.empty() {
		fmt.Fprintln(out, "Nothing of Discobox's was found on this machine.")
		writeUninstallKept(out, plan.kept)
		return
	}
	fmt.Fprintln(out, "This will delete:")
	for _, server := range plan.servers {
		fmt.Fprintf(out, "\n  Stop the running server at %s\n", server.endpoint)
	}
	for _, entry := range plan.entries {
		fmt.Fprintf(out, "\n  %s (%s)\n", entry.path, humanBytes(entry.size))
		if len(entry.what) > 0 {
			fmt.Fprintf(out, "      %s\n", strings.Join(entry.what, "; "))
		}
	}
	if len(plan.ssh.removed) > 0 {
		fmt.Fprintf(out, "\n  %s from %s\n", plural(len(plan.ssh.removed), "Include line", "Include lines"), plan.ssh.target.userConfig.local)
		for _, line := range plan.ssh.removed {
			fmt.Fprintf(out, "      %s\n", line)
		}
	}
	writeUninstallKept(out, plan.kept)
	if executables := runningExecutables(); len(executables) > 0 {
		fmt.Fprintf(out, "\nThe discobox command (%s) is not deleted.\n", executables[len(executables)-1])
	}
}

func writeUninstallKept(out io.Writer, kept []uninstallKept) {
	if len(kept) == 0 {
		return
	}
	fmt.Fprintln(out, "\nNot deleted:")
	for _, k := range kept {
		fmt.Fprintf(out, "\n  %s\n", k.path)
		if k.what != "" {
			fmt.Fprintf(out, "      %s\n", k.what)
		}
		fmt.Fprintf(out, "      not deleted: %s\n", k.reason)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
