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

// resolveIdentity reads the manifest's user out of the environment and fills in
// whatever it left out by asking the OS, never by defaulting it (ADR 0025 §6).
//
// Nothing is invented. A missing uid does not become 0, a missing gid does not
// become the uid, and a primary group given by name is resolved against the
// image's own /etc/group. An account the manifest names but the image lacks is
// created by ensureUser afterwards, which is why resolution tolerates a uid that
// does not resolve yet.
func resolveIdentity() (identity, error) {
	spec := runuser.User{
		Name:          strings.TrimSpace(os.Getenv("DISCOBOX_USER_NAME")),
		Group:         strings.TrimSpace(os.Getenv("DISCOBOX_USER_GROUP")),
		HomeDirectory: strings.TrimSpace(os.Getenv("DISCOBOX_USER_HOME")),
	}
	uid, err := envID("DISCOBOX_USER_UID")
	if err != nil {
		return identity{}, err
	}
	gid, err := envID("DISCOBOX_USER_GID")
	if err != nil {
		return identity{}, err
	}
	spec.UID, spec.GID = uid, gid
	if spec.Empty() && spec.Group == "" {
		// The manifest named nobody. Provision nothing and leave the image's own
		// account in place.
		return identity{}, nil
	}
	if spec.UID == nil {
		return identity{}, errors.New("DISCOBOX_USER_UID is required when a sandbox user is configured")
	}
	// A group given by name resolves here; a missing gid is left to ensureGroup,
	// which creates the group when the image has none. Resolve cannot answer for
	// an account that does not exist yet, so only the group is resolved now.
	if spec.Group != "" {
		if spec.GID != nil {
			return identity{}, errors.New("DISCOBOX_USER_GID and DISCOBOX_USER_GROUP are mutually exclusive")
		}
		resolved, ok := runuser.LookupGroupID(spec.Group)
		if !ok {
			return identity{}, fmt.Errorf("DISCOBOX_USER_GROUP %q is not a group in this image", spec.Group)
		}
		gid := int64(resolved)
		spec.GID = &gid
	}
	if spec.GID == nil {
		// No group was named. The account's own entry knows its default group;
		// if there is no account yet there is nothing to go on, and guessing the
		// uid is exactly the coincidence ADR 0025 §6 forbids.
		found, err := runuser.Resolve(runuser.User{UID: spec.UID})
		if err != nil {
			return identity{}, fmt.Errorf("resolve sandbox user gid: %w", err)
		}
		spec.GID = found.GID
	}
	id := identity{uid: int(*spec.UID), gid: int(*spec.GID), name: spec.Name, home: spec.HomeDirectory, configured: true}
	if id.name == "" || id.home == "" {
		// Fill name and home from the account when it already exists; a fresh
		// account has neither, and ensureUser creates it from what we do have.
		if name, home, err := runuser.NameAndHome(&spec); err == nil {
			if id.name == "" {
				id.name = name
			}
			if id.home == "" {
				id.home = home
			}
		}
	}
	if id.name == "" {
		return identity{}, errors.New("DISCOBOX_USER_NAME is required for a user the image does not already have")
	}
	if id.home == "" {
		id.home = filepath.Join("/home", id.name)
	}
	return id, nil
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
	return b.run("chown", "-R", "--no-dereference", fmt.Sprintf("%d:%d", id.uid, id.gid), id.home)
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
