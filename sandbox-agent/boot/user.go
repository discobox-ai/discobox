package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandboxconfig"
	"github.com/obot-platform/discobox/sandboxuser"
)

// identity is the resolved sandbox user, sourced from the DISCOBOX_USER_* env
// the pool agent injects. It mirrors the values the manifest publishes so the
// init flow, the harness, and exec defaults all use one user.
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
// (which the pool agent forwards as DISCOBOX_USER_*) layered over the image's
// own identity. Precedence and completion belong to runuser; this function
// supplies the layers and declares what it needs (ADR 0033 §1).
//
// What it needs differs between the two cases, and that difference is the whole
// reason boot cannot simply ask for everything. When the manifest names nobody,
// the image's account exists by definition and every field is available. When
// it names somebody, that account may not exist yet -- ensureUser is about to
// create it -- so only the ids can be required, and the descriptive fields are
// asked for separately and allowed to be absent.
func resolveIdentity() (identity, error) {
	manifest, err := manifestUser()
	if err != nil {
		return identity{}, err
	}
	layers := runuser.Layers{Image: runuser.Current(), Manifest: manifest}

	if !sandboxuser.Named(manifest) {
		// The manifest named nobody, so the sandbox runs as whatever the image
		// already is (ADR 0025 §5) -- but callers still need concrete values for
		// it, to build the process environment and to expand %HOME%-templated
		// volumes. Nothing is deferred here: boot runs as PID 1 before anything
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
	// it. Asking without them in `need` is how that expectation is stated.
	if described, err := runuser.Resolve(layers, sandboxuser.Complete); err == nil {
		id.name, id.home = described.Name, described.HomeDirectory
	}
	if id.name == "" {
		id.name = strings.TrimSpace(manifest.Name)
	}
	if id.name == "" {
		return identity{}, errors.New("DISCOBOX_USER_NAME is required for a user the image does not already have")
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
// injected. Absent is absent: an unset id stays nil rather than becoming 0 or
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

// ensureUser creates or aligns the sandbox user/group and grants passwordless
// sudo, mirroring the retired entrypoint.sh. Root needs none of this.
func (b *booter) ensureUser(id identity) error {
	if !id.configured || id.uid == 0 {
		return nil
	}
	groupName, err := b.ensureGroup(id)
	if err != nil {
		return err
	}
	if err := b.ensureAccount(id, groupName); err != nil {
		return err
	}
	if !sudoersNameRE.MatchString(id.name) {
		return fmt.Errorf("DISCOBOX_USER_NAME %q is not safe for sudoers", id.name)
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

func (b *booter) ensureAccount(id identity, groupName string) error {
	if b.exists("id", "-u", id.name) {
		return b.run("usermod", "--uid", strconv.Itoa(id.uid), "--gid", strconv.Itoa(id.gid), "--home", id.home, id.name)
	}
	if out, ok := b.lookup("getent", "passwd", strconv.Itoa(id.uid)); ok {
		existing := firstField(out)
		if existing != "" && existing != id.name {
			if err := b.run("usermod", "--login", id.name, existing); err != nil {
				return err
			}
		}
		return b.run("usermod", "--gid", strconv.Itoa(id.gid), "--home", id.home, id.name)
	}
	return b.run("useradd", "--uid", strconv.Itoa(id.uid), "--gid", groupName, "--home-dir", id.home, "--shell", "/bin/bash", id.name)
}

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
}

// sudoersContent renders the drop-in, separate from writing it so its shape can
// be asserted without touching /etc.
func sudoersContent(name string) string {
	return fmt.Sprintf("Defaults env_keep += \"%s\"\n%s ALL=(ALL) NOPASSWD:ALL\n",
		strings.Join(proxyEnvKeep, " "), name)
}

func (b *booter) writeSudoers(name string) error {
	if err := os.MkdirAll("/etc/sudoers.d", 0o750); err != nil {
		return err
	}
	path := "/etc/sudoers.d/discobox-user"
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
// populates it from /etc/skel, then fixes ownership.
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
	if empty {
		if _, err := os.Stat("/etc/skel"); err == nil {
			if err := b.run("cp", "-aT", "/etc/skel", id.home); err != nil {
				return err
			}
		}
	}
	// Root's home tree stays as shipped; a non-root user owns its whole home.
	if id.uid == 0 {
		return os.Lchown(id.home, id.uid, id.gid)
	}
	return chownTreeOnOwnFilesystem(id.home, id.uid, id.gid)
}

// chownTreeOnOwnFilesystem chowns root and everything under it that lives on
// root's own filesystem, never descending into a volume mounted underneath.
//
// The recursion is here for the directories boot itself creates as root on the
// way to a mountpoint -- ~/.cargo on the way to ~/.cargo/registry, ~/go/pkg on
// the way to ~/go/pkg/mod -- which wireVolume leaves root-owned because
// applyOwnership chowns only the mountpoint. Those live on home's filesystem.
//
// What it must not walk is the volumes themselves. The shared pool cache and
// the source trees are mounted under home, they already have the ownership
// wireVolume/wireSources and the pool agent gave them, and they are large:
// ~4.7*10^5 inodes on a working machine, which cost ~14s of every boot on a
// cold page cache. GNU chown has no --one-file-system, hence the explicit walk.
func chownTreeOnOwnFilesystem(root string, uid, gid int) error {
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	// An unreportable device number means every path compares equal below, so
	// the walk degrades to covering the whole tree rather than skipping it.
	rootDev, haveDev := fileDevice(fi)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != root && haveDev {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if dev, ok := fileDevice(info); ok && dev != rootDev {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
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
// Runs after seedHome, which recursively chowns the home tree: a file written
// before that would be correctly owned by luck rather than by construction, and
// one written after has to say so itself. Writing via git also means an existing
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
}

func newBooter() *booter {
	ctx := context.Background()
	b := &booter{}
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
