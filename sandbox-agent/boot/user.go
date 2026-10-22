package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxuser"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

// identity is the resolved sandbox user, sourced from the manifest's user:
// sandbox.json's own `user` (Wire and Provision), or the DISCOBOX_USER_* env the
// pool agent injects into the export container (Export), which has no
// bootstrap where the agent reads it. Both are the one user the manifest
// publishes, so boot, the harness, and exec defaults all use one user.
//
// configured reports whether the manifest asked for a specific user at all.
// When it did not, boot provisions no account and the sandbox runs as whatever
// the image already is (ADR 0025 §5) -- absent must not silently become root.
type identity struct {
	uid        int
	gid        int
	name       string
	home       string
	configured bool
}

var sudoersNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*\$?$`)

// resolveIdentity works out who this sandbox runs as, from the manifest's user
// layered over the image's own identity. Precedence and completion belong to runuser; this function
// supplies the layers and declares what it needs (ADR 0033 §1).
//
// What it needs differs between the two cases, and that difference is the whole
// reason boot cannot simply ask for everything. When the manifest names nobody,
// the image's account exists by definition and every field is available. When
// it names somebody, that account may not exist yet -- ensureUser is about to
// create it -- so only the ids can be required, and the descriptive fields are
// asked for separately and allowed to be absent.
func resolveIdentity(manifest *runuser.User) (identity, error) {
	layers := runuser.Layers{Image: runuser.Current(), Manifest: manifest}

	if !sandboxuser.Named(manifest) {
		// The manifest named nobody, so the sandbox runs as whatever the image
		// already is (ADR 0025 §5) -- but callers still need concrete values for
		// it, to build the process environment and to expand %HOME%-templated
		// volumes. Nothing is deferred here: boot runs as root before anything
		// has called setuid, so the image's account is this process's own and
		// /etc/passwd answers for all of it.
		resolved, err := runuser.Resolve(layers, sandboxuser.Complete)
		if err != nil {
			return identity{}, fmt.Errorf("resolve the image's own user: %w", err)
		}
		return identity{
			uid:  int(*resolved.UID),
			gid:  int(*resolved.GID),
			name: resolved.Name,
			home: resolved.HomeDirectory,
		}, nil
	}

	// Only the ids are required. A gid the manifest did not give is taken from
	// the account's own entry, never from the uid -- uid == gid is a useradd
	// coincidence, and guessing it runs the process under whatever group happens
	// to hold that number (ADR 0025 §6).
	resolved, err := runuser.Resolve(layers, sandboxuser.FieldUID|sandboxuser.FieldGID)
	if err != nil {
		return identity{}, fmt.Errorf("resolve sandbox user: %w", err)
	}
	id := identity{uid: int(*resolved.UID), gid: int(*resolved.GID), configured: true}

	// Name and home come from the account when it already exists. A fresh one
	// has neither, which is not an error: boot is the thing that gives them to
	// it. Asking without them in `need` is how that expectation is stated. An
	// account in the way under another name is not this one, and runuser
	// completes nothing from it.
	if described, err := runuser.Resolve(layers, sandboxuser.Complete); err == nil {
		id.name, id.home = described.Name, described.HomeDirectory
	}
	if id.name == "" {
		id.name = strings.TrimSpace(manifest.Name)
	}
	if id.name == "" {
		return identity{}, errors.New("the manifest's user needs a name: the image has no account for its uid")
	}
	if id.home == "" {
		id.home = strings.TrimSpace(manifest.HomeDirectory)
	}
	if id.home == "" {
		// The account is being created here, so this is a decision about where
		// its home goes rather than a guess about where it already is. Nothing
		// outside the sandbox may make this choice (ADR 0033 §5); boot may,
		// because it is the thing that creates the directory.
		id.home = filepath.Join("/home", id.name)
	}
	return id, nil
}

// manifestUser reads the manifest's user out of the environment the pool agent
// injected into the export container. Absent is absent: an unset id stays nil rather than becoming 0 or
// borrowing the other one.
func manifestUser() (*runuser.User, error) {
	out := &runuser.User{
		Name:          strings.TrimSpace(os.Getenv("DISCOBOX_USER_NAME")),
		GroupName:     strings.TrimSpace(os.Getenv("DISCOBOX_USER_GROUP")),
		HomeDirectory: strings.TrimSpace(os.Getenv("DISCOBOX_USER_HOME")),
	}
	uid, err := envID("DISCOBOX_USER_UID")
	if err != nil {
		return nil, err
	}
	gid, err := envID("DISCOBOX_USER_GID")
	if err != nil {
		return nil, err
	}
	out.UID, out.GID = uid, gid
	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("DISCOBOX_USER_GID and DISCOBOX_USER_GROUP: %w", err)
	}
	return out, nil
}

// envID reads an optional numeric id. Absent means absent -- it never falls back
// to another field's value.
func envID(key string) (*int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s %q must be numeric", key, raw)
	}
	return &parsed, nil
}

// ensureUser makes the sandbox's account and grants it passwordless sudo.
// Root needs none of this.
//
// An existing account is never changed in place: one that is already exactly
// the sandbox's -- its name, uid, gid and home, made by an earlier boot -- is
// kept, and any other holding the sandbox's uid or name is deleted before the
// account is created, as if the box had never had one (ADR 26-10-16-423 §14).
// A provider's image commonly has an account at uid 1000, the uid every macOS
// and Windows client sends; that and any other account in the way are
// deleted whenever boot finds one.
func (b *booter) ensureUser(id identity) error {
	if !id.configured || id.uid == 0 {
		return nil
	}
	// Refused before anything is deleted. Root is uid 0, which needs no
	// account (ADR 0141); a root at another uid would have to be made by
	// deleting the real one.
	if id.name == "root" {
		return fmt.Errorf("the sandbox's account cannot be named root with uid %d: root is uid 0", id.uid)
	}
	if !sudoersNameRE.MatchString(id.name) {
		return fmt.Errorf("user name %q is not safe for sudoers", id.name)
	}
	// Before the group: userdel takes the deleted account's own group with
	// it, which may be the group at the sandbox's gid.
	kept, err := b.clearAccountsInTheWay(id)
	if err != nil {
		return err
	}
	groupName, err := b.ensureGroup(id)
	if err != nil {
		return err
	}
	if !kept {
		if err := b.run("useradd", "--uid", strconv.Itoa(id.uid), "--gid", groupName, "--home-dir", id.home, "--shell", "/bin/bash", id.name); err != nil {
			return err
		}
	}
	return b.writeSudoers(id.name)
}

func (b *booter) ensureGroup(id identity) (string, error) {
	if out, ok := b.lookup("getent", "group", strconv.Itoa(id.gid)); ok {
		return firstField(out), nil
	}
	if _, ok := b.lookup("getent", "group", id.name); ok {
		if err := b.run("groupmod", "--gid", strconv.Itoa(id.gid), id.name); err != nil {
			return "", err
		}
		return id.name, nil
	}
	if err := b.run("groupadd", "--gid", strconv.Itoa(id.gid), id.name); err != nil {
		return "", err
	}
	return id.name, nil
}

// passwdEntry is the fields of an /etc/passwd line boot decides by.
type passwdEntry struct {
	name string
	uid  int
	gid  int
	home string
}

func parsePasswdEntry(line string) (passwdEntry, error) {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Split(line, ":")
	if len(fields) < 6 {
		return passwdEntry{}, fmt.Errorf("passwd entry %q has %d fields, want 7", line, len(fields))
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return passwdEntry{}, fmt.Errorf("passwd entry %q: uid: %w", line, err)
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return passwdEntry{}, fmt.Errorf("passwd entry %q: gid: %w", line, err)
	}
	return passwdEntry{name: fields[0], uid: uid, gid: gid, home: fields[5]}, nil
}

// clearAccountsInTheWay deletes every account holding the sandbox's uid or
// name, and reports whether the sandbox's own account is already there and
// was kept.
func (b *booter) clearAccountsInTheWay(id identity) (kept bool, err error) {
	var holders []passwdEntry
	for _, key := range []string{strconv.Itoa(id.uid), id.name} {
		out, ok := b.lookup("getent", "passwd", key)
		if !ok {
			continue
		}
		entry, err := parsePasswdEntry(out)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(holders, func(h passwdEntry) bool { return h.name == entry.name }) {
			holders = append(holders, entry)
		}
	}
	for _, holder := range holders {
		if holder.name == id.name && holder.uid == id.uid && holder.gid == id.gid &&
			filepath.Clean(holder.home) == filepath.Clean(id.home) {
			kept = true
			continue
		}
		if err := b.deleteAccount(holder); err != nil {
			return false, err
		}
	}
	return kept, nil
}

// loginUsersDir is where logind keeps a file per user it has sessions for. A
// variable so a test can say who is logged in.
var loginUsersDir = "/run/systemd/users"

// deleteAccount ends an account's processes, deletes it, and removes its home
// where that is safe (removeAccountHome).
//
// A provider's login user has processes -- boxd's exec runs as boxd -- and
// userdel refuses a user that is logged in (PR #180). logind's sessions are
// ended first, only for a user it has some (at Wire, before sysinit, there is
// no logind to ask), then anything left at the uid is killed. --force deletes
// even so, since a killed process can still be waiting to be reaped.
func (b *booter) deleteAccount(holder passwdEntry) error {
	if holder.uid == 0 {
		return fmt.Errorf("account %q is uid 0 and holds the sandbox account's name or uid: root is never deleted", holder.name)
	}
	uid := strconv.Itoa(holder.uid)
	if fileExists(filepath.Join(loginUsersDir, uid)) {
		// Not checked: whatever it leaves running, pkill ends next.
		_ = b.run("loginctl", "terminate-user", uid)
	}
	// pkill exits 1 when nothing ran at the uid, the usual answer.
	_ = b.exists("pkill", "--signal", "KILL", "--uid", uid)
	if err := b.run("userdel", "--force", holder.name); err != nil {
		return err
	}
	return removeAccountHome(holder)
}

// mountInfoPath lists this process's mounts. A variable so a test can say
// what is mounted.
var mountInfoPath = "/proc/self/mountinfo"

// removeAccountHome removes a deleted account's home, so the sandbox's account
// starts from /etc/skel with nothing of the old one kept, unless it is not
// that account's to lose:
//
//   - a mount point, or a directory with anything mounted beneath it, which
//     a recursive removal would empty -- a driver that mounts the sandbox's
//     persistent home before boot runs is the case it keeps;
//   - a directory the account does not own, as a system account's home
//     (daemon's /usr/sbin) is not; and anything not a directory.
//
// A plain directory at the path the sandbox's home will be is removed like
// any other: left, it would hold files of a uid that no longer exists, and
// seedHome would give it no skeleton, since it seeds only an empty home.
func removeAccountHome(holder passwdEntry) error {
	if !filepath.IsAbs(holder.home) {
		return nil
	}
	home := filepath.Clean(holder.home)
	if home == "/" {
		return nil
	}
	info, err := os.Lstat(home)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if owner, _, ok := fileOwner(info); !ok || owner != holder.uid {
		return nil
	}
	mounts, err := mountPoints(mountInfoPath)
	if err != nil {
		return fmt.Errorf("remove the home of deleted account %q: %w", holder.name, err)
	}
	if slices.ContainsFunc(mounts, func(mount string) bool { return within(home, mount) }) {
		return nil
	}
	return os.RemoveAll(home)
}

// mountPoints reads the mount points out of a mountinfo file: the fifth
// field, with the octal escapes the kernel writes for space, tab, newline
// and backslash undone.
func mountPoints(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mounts = append(mounts, filepath.Clean(mountInfoUnescaper.Replace(fields[4])))
	}
	return mounts, nil
}

var mountInfoUnescaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// ensureAdditionalGroups adds the sandbox user to supplementary OS groups the
// image declared (e.g. "docker"), alongside its own primary group. It runs
// after the account exists, once the image's declared groups are known from
// sandbox.json — unlike group/account creation, this can't happen any earlier
// in provision. A group the image names but the base OS doesn't have (e.g. a
// harness Dockerfile forgetting to install the package that creates it) is
// skipped rather than failing sandbox boot over a foreseeable image mistake.
// Root already has every access these groups would grant, so it has none of
// this to do.
func (b *booter) ensureAdditionalGroups(id identity, groups []string) error {
	if id.uid == 0 || len(groups) == 0 {
		return nil
	}
	present := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, ok := b.lookup("getent", "group", group); !ok {
			continue
		}
		present = append(present, group)
	}
	if len(present) == 0 {
		return nil
	}
	return b.run("usermod", "--append", "--groups", strings.Join(present, ","), id.name)
}

// proxyEnvKeep are the variables sudo must carry from the sandbox user to
// root. They mirror what pool-agent's EnsureSandboxMaterial injects; a name
// listed here that is unset is simply not passed on.
var proxyEnvKeep = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "PIP_CERT",
	"NODE_USE_ENV_PROXY",
}

// sudoersContent renders the drop-in, separate from writing it so its shape can
// be asserted without touching /etc.
func sudoersContent(name string) string {
	return fmt.Sprintf("Defaults env_keep += \"%s\"\n%s ALL=(ALL) NOPASSWD:ALL\n",
		strings.Join(proxyEnvKeep, " "), name)
}

// sudoersPath is the sandbox account's sudoers drop-in. A variable so a test
// does not write the machine's own.
var sudoersPath = "/etc/sudoers.d/discobox-user"

func (b *booter) writeSudoers(name string) error {
	if err := os.MkdirAll(filepath.Dir(sudoersPath), 0o750); err != nil {
		return err
	}
	path := sudoersPath
	// sudo runs with env_reset, so the sandbox's proxy-trust variables are
	// stripped on the way to root and anything run under sudo tries to reach
	// the network directly -- which a sandbox has no route for. env_keep is the
	// targeted lever: it applies only to sudo and preserves the caller's own
	// values, which boot and the runc wrapper already set correctly.
	//
	// Deliberately not /etc/environment: that is read by PAM for every login
	// session regardless of context, cannot be scoped, and would put proxy
	// settings in front of processes that must not inherit them.
	content := sudoersContent(name)
	//nolint:gosec // sudoers drop-ins must be mode 0440; sudo refuses more permissive files.
	if err := os.WriteFile(path, []byte(content), 0o440); err != nil {
		return err
	}
	return b.run("visudo", "-cf", path)
}

// seedHome creates the home directory (a mounted data volume) and, when empty,
// populates it from /etc/skel, giving the copy to the user.
//
// It chowns only what it creates: the home directory itself and the skeleton it
// copied. It never walks a home that was already there. That home holds the
// volumes and sources mounted under it -- the pool cache, module caches,
// checkouts, unbounded and all bound from one host filesystem, so no device
// check can tell them apart. wireVolume and seedWorkingRoot give away the
// directories they invent on the way to a mountpoint (mkdirAllOwned) and
// applyOwnership the mountpoints; the user owns what it wrote; and what an
// image shipped there keeps the image's ownership, which is the image's
// statement (#234).
func (b *booter) seedHome(id identity) error {
	if err := os.MkdirAll(id.home, 0o755); err != nil {
		return err
	}
	if err := os.Chown(id.home, id.uid, id.gid); err != nil {
		return err
	}
	empty, err := dirEmpty(id.home)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	if !dirExists("/etc/skel") {
		return nil
	}
	if err := b.run("cp", "-aT", "/etc/skel", id.home); err != nil {
		return err
	}
	// Root's skeleton stays as shipped: cp -a kept root's ownership already.
	if id.uid == 0 {
		return nil
	}
	return chownTree(id.home, id.uid, id.gid)
}

// chownTree chowns root and everything under it. seedHome calls it only on a
// home it has just filled from an empty directory, so what it walks is the
// skeleton copy and nothing mounted.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		//nolint:gosec // The tree is the sandbox user's own home; Lchown avoids following symlinks out of it.
		return os.Lchown(p, uid, gid)
	})
}

// gitConfigArgv builds a `git config` invocation pinned to the sandbox user's
// own config, run through env(1) because the booter's runner takes no
// environment of its own.
//
// Both variables are set, and neither is redundant. GIT_CONFIG_GLOBAL names the
// exact file to write, so the destination cannot drift to
// $XDG_CONFIG_HOME/git/config depending on what the image happens to export.
// HOME covers everything else git resolves against the user rather than against
// that one path.
func gitConfigArgv(id identity, args ...string) []string {
	argv := []string{
		"env",
		"HOME=" + id.home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(id.home, ".gitconfig"),
		"git", "config",
	}
	return append(argv, args...)
}

// gitConfigSet reports whether git already resolves a value for key. The read is
// deliberately not scoped to --global: an image that shipped an identity in
// /etc/gitconfig has an answer, and boot is not entitled to a second opinion
// about it.
//
// A key set to the empty string counts as unset. Git will happily store one, and
// it commits no better than a missing key does.
func (b *booter) gitConfigSet(id identity, key string) bool {
	argv := gitConfigArgv(id, "--get", key)
	out, ok := b.lookup(argv[0], argv[1:]...)
	return ok && strings.TrimSpace(out) != ""
}

// seedGitConfig seeds the sandbox user's git identity, so work done inside the
// sandbox is attributed to whoever asked for it rather than to git's
// user@hostname fallback.
//
// It fills in each key independently, and only where git has no answer already
// (ADR 0042 §4) -- it does not ask whether ~/.gitconfig exists. Those are
// different questions: a file holding aliases and a signing key but no identity
// is exactly the case that most needs seeding, and treating its existence as
// "already configured" would skip the sandbox that needed this most. Per-key
// also means a user who set only user.email keeps it and still gets a name.
//
// Git itself is the authority on what is set, rather than boot parsing the file:
// the same rule the CLI follows when reading the identity on the way in.
//
// Runs after seedHome, which fills only an empty home from /etc/skel and
// chowns nothing it did not copy, so this file has to say who owns it itself.
// Writing via git also means an existing
// file is replaced through a lock-and-rename, which leaves it owned by boot --
// so the chown below is what a rewrite needs, not only a fresh file.
func (b *booter) seedGitConfig(id identity, git sandboxconfig.GitIdentity) error {
	if !git.Configured() {
		return nil
	}
	// A sandbox whose image ships no git has nothing to configure, and must not
	// fail to boot over it -- the same rule ensureAdditionalGroups applies to a
	// group the image never created.
	if !b.exists("git", "--version") {
		return nil
	}
	wrote := false
	for _, entry := range []struct{ key, value string }{
		{"user.name", strings.TrimSpace(git.UserName)},
		{"user.email", strings.TrimSpace(git.UserEmail)},
	} {
		if entry.value == "" || b.gitConfigSet(id, entry.key) {
			continue
		}
		argv := gitConfigArgv(id, "--global", entry.key, entry.value)
		if err := b.run(argv[0], argv[1:]...); err != nil {
			return err
		}
		wrote = true
	}
	if !wrote {
		return nil
	}
	return os.Chown(filepath.Join(id.home, ".gitconfig"), id.uid, id.gid)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dirEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func dirNonEmpty(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return true, nil
	}
	empty, err := dirEmpty(path)
	if err != nil {
		return false, err
	}
	return !empty, nil
}

func firstField(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// booter runs external commands and logs; split out so tests can substitute a
// recording runner.
type booter struct {
	run    func(name string, args ...string) error
	lookup func(name string, args ...string) (string, bool)
	exists func(name string, args ...string) bool
	// wireFeatures provisions the box's features' wiring before the account
	// exists (provision.Runner.Wire); nil wires none.
	wireFeatures func(logger *slog.Logger, features map[string]sandboxconfig.Feature)
	// statusPath is the box's provision.json, which says which features are
	// off; empty turns none off.
	statusPath string
}

// off reports whether provisioning turned feature off, so a boot step that
// wires it is skipped (ADR 26-10-16-423 §4).
func (b *booter) off(feature string) bool {
	return b.statusPath != "" && provisionspec.FeatureOff(b.statusPath, feature)
}

func newBooter() *booter {
	ctx := context.Background()
	b := &booter{statusPath: provisionspec.StatusPath}
	b.wireFeatures = func(logger *slog.Logger, features map[string]sandboxconfig.Feature) {
		// Not fatal: what failed is in provision.json, and the agent's run
		// tries again and fails readiness if it still fails.
		tracker := provision.NewTracker(provisionspec.StatusPath)
		if err := provision.Boot(ctx, logger, tracker, features, true); err != nil {
			logger.Warn("provision the box's wiring", "error", err)
		}
	}
	b.run = func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil
	}
	b.lookup = func(name string, args ...string) (string, bool) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			return "", false
		}
		return string(out), true
	}
	b.exists = func(name string, args ...string) bool {
		return exec.CommandContext(ctx, name, args...).Run() == nil
	}
	return b
}

// installFile writes content with mode, creating parents. Used for systemd
// drop-ins the desktop wiring needs.
func installFile(path string, mode os.FileMode, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}
