package boot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// identity is the resolved sandbox user, sourced from the DISCOBOX_USER_* env
// the worker injects. It mirrors the values the manifest publishes so the init
// flow, the harness, and exec defaults all use one user.
type identity struct {
	uid  int
	gid  int
	name string
	home string
}

var sudoersNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*\$?$`)

func resolveIdentity() (identity, error) {
	id := identity{name: envOr("DISCOBOX_USER_NAME", "root"), home: envOr("DISCOBOX_USER_HOME", "/root")}
	uidStr := envOr("DISCOBOX_USER_UID", "0")
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return identity{}, fmt.Errorf("DISCOBOX_USER_UID %q must be numeric", uidStr)
	}
	id.uid = uid
	gidStr := envOr("DISCOBOX_USER_GID", uidStr)
	gid, err := strconv.Atoi(gidStr)
	if err != nil {
		return identity{}, fmt.Errorf("DISCOBOX_USER_GID %q must be numeric", gidStr)
	}
	id.gid = gid
	if id.uid == 0 {
		id.name = "root"
		id.home = envOr("DISCOBOX_USER_HOME", "/root")
	}
	return id, nil
}

// ensureUser creates or aligns the sandbox user/group and grants passwordless
// sudo, mirroring the retired entrypoint.sh. Root needs none of this.
func (b *booter) ensureUser(id identity) error {
	if id.uid == 0 {
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

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
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
