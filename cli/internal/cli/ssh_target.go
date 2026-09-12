package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// sshPath is one file this CLI writes for an ssh client to read, in the two
// spellings that file has: `local` is how this process opens it, `client` is
// how the ssh that reads it spells the same file.
//
// The two are the same string everywhere but across the WSL boundary, where
// this process writes /mnt/c/Users/x/AppData/Local/… and the Windows ssh.exe
// that reads it knows the file as C:\Users\x\AppData\Local\….
type sshPath struct {
	local  string
	client string
}

// samePath is an sshPath for a file both sides spell the same way.
func samePath(p string) sshPath { return sshPath{local: p, client: p} }

// sshTarget is the OpenSSH installation an emitted config is written for: where
// its files go, how a path inside it is spelled, and what its ProxyCommand has
// to be to reach this CLI.
//
// There is normally one — this machine's — and on WSL there are two. Which of
// them matters is decided by the program that will drive ssh: VS Code launched
// from WSL is usually the Windows build, and a Windows VS Code runs Windows
// ssh.exe, which reads the Windows user's ssh_config, cannot open a path in the
// distribution's filesystem, and cannot execute a Linux binary. See ADR 0074.
type sshTarget struct {
	// windows reports that the reading client is Windows OpenSSH. It decides
	// how paths are spelled and how the ProxyCommand is quoted, and it is true
	// both for a CLI running on Windows and for one running in WSL writing for
	// the Windows side.
	windows bool
	// wslDistro is the distribution the ProxyCommand re-enters to reach this
	// CLI, and is empty unless this is a WSL process writing for Windows. It
	// doubles as the flag for that case: only then do the two spellings of a
	// path differ, and only then does the identity have to be mirrored.
	wslDistro string
	// state is the state directory the managed files live under, and
	// userConfig the ssh_config that gains an Include of them.
	state      sshPath
	userConfig sshPath
	// tools is what the Windows side had to be asked for before anything could
	// be written there: where icacls is, and what this user is called over
	// there. Resolved with the folders beside it and empty everywhere else,
	// since nothing but a WSL process writing for Windows shells into Windows
	// at all.
	tools windowsTools
	// optional says that failing to write for this target is worth reporting
	// and no reason to fail the work that asked for the write. It is the
	// Windows side of a WSL machine and nothing else: everything there runs
	// through interop, a Windows profile and a Windows ACL, and none of it is
	// a reason to lose the config for the ssh this shell is running — least of
	// all in the middle of a run that has already made a discobox to attach
	// to. `tools vscode` launching a Windows editor is the one caller with
	// nothing left to connect through, and clears it. See ADR 0102.
	optional bool
}

// localSSHTarget is the ssh this machine runs: the CLI's own state directory,
// the user's own ~/.ssh/config, and this executable as the ProxyCommand.
func localSSHTarget() (sshTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sshTarget{}, err
	}
	return sshTarget{
		windows:    runtime.GOOS == "windows",
		state:      samePath(cliStateDir()),
		userConfig: samePath(filepath.Join(home, ".ssh", "config")),
	}, nil
}

// windowsSSHTarget is the Windows ssh a WSL process writes for: files under the
// Windows user's %LOCALAPPDATA%, an Include in the Windows user's
// %USERPROFILE%\.ssh\config, and a ProxyCommand that re-enters this
// distribution.
//
// Windows is asked where its own folders are rather than being assumed to keep
// them under C:\Users\<name>: both can be redirected, and a wrong guess writes
// a config nothing reads.
func windowsSSHTarget(ctx context.Context) (sshTarget, error) {
	distro := strings.TrimSpace(os.Getenv(wslDistroEnv))
	if distro == "" {
		return sshTarget{}, fmt.Errorf("cannot tell which WSL distribution this is (%s is unset), "+
			"so there is no ProxyCommand Windows could run to reach this CLI", wslDistroEnv)
	}
	target := sshTarget{windows: true, wslDistro: distro}
	localAppData, err := wslWindowsFolder(ctx, "LOCALAPPDATA")
	if err != nil {
		return sshTarget{}, err
	}
	// Asked here rather than per file: every file written for this target is
	// narrowed with the same icacls under the same name, and each question
	// costs a Windows process launch.
	target.tools, err = resolveWindowsTools(ctx)
	if err != nil {
		return sshTarget{}, err
	}
	profile, err := wslWindowsFolder(ctx, "USERPROFILE")
	if err != nil {
		return sshTarget{}, err
	}
	target.state = target.join(localAppData, "discobox", "cli")
	target.userConfig = target.join(profile, ".ssh", "config")
	return target, nil
}

// machineSSHTargets is every OpenSSH installation on this machine, which is
// what a written config has to reach: this one always, and on WSL the Windows
// one as well.
//
// Both, rather than whichever one a particular command happens to need. A WSL2
// machine really does have two, the programs that drive ssh are spread across
// both — a Windows VS Code or JetBrains Gateway on one side, `git` and `scp` on
// the other — and a config that exists on only one side is a machine that works
// until you reach for the other tool. Writing both also means no command is the
// only way to produce one of them, which is what `tools vscode` had become.
//
// The Windows side needs interop to resolve at all, so its failure travels on
// the value rather than being raised: `admin ssh-config --write` has still
// written a usable config for this side and says what it could not do, while an
// editor command launching a Windows editor — `tools vscode` or `tools zed` —
// cannot proceed and turns it into an error (editorFamily.sshTargets).
func machineSSHTargets(ctx context.Context) (machineTargets, error) {
	local, err := localSSHTarget()
	if err != nil {
		return machineTargets{}, fmt.Errorf("locate this machine's own ssh: %w", err)
	}
	targets := machineTargets{all: []sshTarget{local}}
	if !isWSL() {
		return targets, nil
	}
	// Recorded on the value either way, and added when there was nothing to
	// record: not resolving the Windows side is a thing this answer carries,
	// never a thing it fails on.
	windows, windowsErr := windowsSSHTarget(ctx)
	targets.windowsErr = windowsErr
	if windowsErr == nil {
		windows.optional = true
		targets.all = append(targets.all, windows)
	}
	return targets, nil
}

// machineTargets is what this machine has to write for, and what it could not
// work out about the installation that may be missing.
//
// The two are separate values because their failures are different failures.
// This machine's own ssh is the one every caller is writing for and the one the
// shell asking is running: not having it ends the work. The Windows side of a
// WSL machine is a second installation, and not having it leaves the first
// perfectly usable.
//
// They came back as one error return once, and every caller read that error as
// the Windows one — so a machine whose home directory could not be resolved
// reported "not writing the Windows ssh_config", carried on with no targets at
// all, and then wrote nothing while reporting success, printed a config out of
// an empty slice, or indexed the first of no configs.
type machineTargets struct {
	// all is every installation to write for, this machine's own first. It is
	// empty only when the error beside it is not nil, and two callers index it
	// on the strength of that: `admin ssh-config`'s all[:1] and the built[0]
	// that sandboxSSHRemote hands back. So a caller that reports the error and
	// carries on anyway panics at those two, and writes nothing at all at a
	// third that only passes the slice on — which is the whole reason this
	// error is not one to report and continue past. Both beat the alternative:
	// there is no config worth writing for a machine with no ssh on it, and a
	// shape that handed back a zero sshTarget instead of nothing would write
	// one to paths made of empty strings.
	all []sshTarget
	// windowsErr is why the Windows side is not in all, on a WSL machine where
	// it could not be resolved. It is nil everywhere else, including on a
	// machine that is not WSL and has no second installation to miss.
	windowsErr error
}

// acrossWSL reports that this target's files are written through the WSL
// boundary: opened here by their /mnt path and read on the other side by their
// drive path.
func (t sshTarget) acrossWSL() bool { return t.wslDistro != "" }

// join extends a path in both spellings at once. The local half is joined the
// way this process opens files, the client half the way the reading ssh spells
// them, which is the only reason the two can end up different.
func (t sshTarget) join(base sshPath, elems ...string) sshPath {
	joined := sshPath{local: filepath.Join(append([]string{base.local}, elems...)...)}
	if t.windows {
		joined.client = strings.TrimRight(base.client, `\`) + `\` + strings.Join(elems, `\`)
	} else {
		joined.client = joined.local
	}
	return joined
}

// dir is the directory a path is in, in both spellings — the inverse of join
// for one element, for the callers that have a file and need the directory it
// sits in on the side that reads it.
func (t sshTarget) dir(p sshPath) sshPath {
	parent := sshPath{local: filepath.Dir(p.local)}
	parent.client = parent.local
	if slash := strings.LastIndex(p.client, `\`); t.windows && slash > 0 {
		parent.client = p.client[:slash]
	}
	return parent
}

// restrict gives a file this CLI wrote the permissions the ssh that reads it
// insists on, on the side that reads it. Locally that is restrictToUser — the
// ACL on Windows, and nothing on Unix, where the mode the file was written with
// already said it. Across the boundary only Windows can set an ACL, so it is
// set with icacls (windowsTools.restrictFile).
//
// Every file this CLI writes for an ssh to read goes through it. Windows
// OpenSSH checks the ssh_config it reads, every file that config Includes, and
// every key it opens, and refuses the whole config over any one of them.
func (t sshTarget) restrict(ctx context.Context, p sshPath) error {
	if t.acrossWSL() {
		return t.tools.restrictFile(ctx, p.client)
	}
	return restrictToUser(p.local)
}

// The written artifacts live beside the generated key, under the state
// directory of whichever side reads them, one directory per project.
func (t sshTarget) sshDir() sshPath { return t.join(t.state, "ssh") }

func (t sshTarget) configPath(projectID string) sshPath {
	return t.join(t.sshDir(), projectID, "config")
}

func (t sshTarget) knownHostsPath(projectID string) sshPath {
	return t.join(t.sshDir(), projectID, "known_hosts")
}

// proxyCommandLine is the `ProxyCommand` line an emitted stanza carries: this
// executable, invoked with the endpoint this run was pointed at.
//
// The path is absolute rather than `discobox`, because ssh runs the command
// through a shell with whatever environment its caller had — and the caller is
// often a GUI editor whose PATH is not the shell's. --server is passed for the
// same reason: DISCOBOX_SERVER may not be set where ssh runs, and the config
// has to keep meaning what it meant when it was written.
//
// For Windows the same invocation is wrapped in `wsl.exe`: ssh.exe cannot
// execute a Linux binary, so the command re-enters the distribution this CLI is
// installed in and runs it there.
//
// The quoting is the whole difficulty, and it is not %COMSPEC%'s. cmd.exe hands
// the line on with its quotes intact, and `wsl.exe` does not strip them from
// the words it consumes: quoting each word the way a native Windows
// ProxyCommand is quoted gets the Linux side an execvp of a program whose name
// begins with a double quote. It fails, wsl.exe reports that failure in UTF-16
// on stdout, and ssh — which expects an identification string there — rejects
// the connection with "banner line contains invalid characters" (ADR 0078 §1).
//
// So the words are quoted for the shell that actually parses them. The command
// goes to `sh -c` as a single double-quoted argument, which cmd.exe and
// wsl.exe pass across whole, and inside it the path and the endpoint carry
// POSIX single quotes, which sh strips. Nothing in that argument is a double
// quote, so there is no nesting to get wrong. `exec` because the shell has
// nothing left to do afterwards, and the stdio ssh hands it has to reach the
// splice unbroken.
func (t sshTarget) proxyCommandLine(serverURL string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this executable for the ssh_config ProxyCommand: %w", err)
	}
	if !t.acrossWSL() {
		return shellQuote(executable, t.windows) + " --server " + shellQuote(serverURL, t.windows) + " admin ssh-proxy", nil
	}
	script := "exec " + shellQuote(executable, false) + " --server " + shellQuote(serverURL, false) + " admin ssh-proxy"
	// The distribution name is bare for the same reason: it is a word wsl.exe
	// reads itself, and a quoted one would name a distribution that does not
	// exist.
	return "wsl.exe -d " + t.wslDistro + " -e sh -c " + shellQuote(script, true), nil
}

// mirrorSSHIdentity returns the key path the stanzas should name, copying the
// key across the boundary first when the ssh that reads them is on the other
// side of it.
//
// Windows OpenSSH opens the identity file itself: a WSL path means nothing to
// it, and the \\wsl.localhost share it could name instead fails the owner and
// permission check it makes before reading a private key. So on a WSL machine
// the key exists on both sides, and the copy is rewritten on every run so a
// rotated or re-enrolled key never leaves a stale one behind.
//
// What the copy cannot do is inherit an acceptable ACL. ssh reads a private key
// under three principals — its owner, SYSTEM and Administrators — and neither
// way of putting the file there lands on those: a file written from this side
// carries an explicit ACE for `S-1-5-32` that WSL puts on everything it creates
// on a drive mount, and one created on the Windows side inherits whatever the
// profile grants, where a group with read access is enough to be refused. So
// the ACL is set, not inherited, which from here means icacls
// (windowsTools.restrictFile). See ADR 0102 §1.
//
// A copy this cannot vouch for is removed again — a wide ACL, but equally an
// icacls that could not be run or did not answer. It is on the Windows
// filesystem only to be read by ssh.exe, the permissions it has are the ones
// nothing confirmed, and the caller may well turn this error into a warning and
// carry on (ADR 0102 §3) — which would leave an enrolled private key sitting
// there under exactly what this exists to check.
func (t sshTarget) mirrorSSHIdentity(ctx context.Context, source string) (sshPath, error) {
	if !t.acrossWSL() {
		return samePath(source), nil
	}
	mirrored := t.join(t.sshDir(), filepath.Base(source))
	if err := ensureStateDir(filepath.Dir(mirrored.local)); err != nil {
		return sshPath{}, fmt.Errorf("create Windows SSH directory: %w", err)
	}
	for _, suffix := range []string{"", ".pub"} {
		data, err := os.ReadFile(source + suffix)
		if err != nil {
			if suffix == ".pub" && os.IsNotExist(err) {
				// ssh derives the public half from the private one; a key
				// enrolled by hand may simply not have it beside it.
				continue
			}
			return sshPath{}, fmt.Errorf("read SSH identity: %w", err)
		}
		if err := os.WriteFile(mirrored.local+suffix, data, 0o600); err != nil {
			return sshPath{}, fmt.Errorf("copy SSH identity for Windows: %w", err)
		}
		if suffix == "" {
			// Only the private half. The public one is public, and ssh reads
			// it without an opinion about who else can.
			if err := t.tools.restrictFile(ctx, mirrored.client); err != nil {
				if removeErr := os.Remove(mirrored.local); removeErr != nil {
					return sshPath{}, errors.Join(err, fmt.Errorf("remove the copy: %w", removeErr))
				}
				return sshPath{}, err
			}
		}
	}
	return mirrored, nil
}

// clean normalizes a path for comparison with another spelled by the same
// client. Windows paths are compared in their slash form, because this process
// may be the one that has to compare them and filepath knows only its own
// separator.
func (t sshTarget) clean(p string) string {
	if t.windows {
		return path.Clean(strings.ReplaceAll(p, `\`, "/"))
	}
	return filepath.Clean(p)
}

// samePathAs reports whether two paths name the same file for this client.
// Windows filenames are case-insensitive; nothing else here is.
func (t sshTarget) samePathAs(a, b string) bool {
	if t.windows {
		return strings.EqualFold(t.clean(a), t.clean(b))
	}
	return t.clean(a) == t.clean(b)
}

// within reports whether path is dir or sits under it.
func (t sshTarget) within(dir, p string) bool {
	cleanDir, cleanPath := t.clean(dir), t.clean(p)
	if t.samePathAs(cleanDir, cleanPath) {
		return true
	}
	prefix := strings.TrimSuffix(cleanDir, "/") + "/"
	if t.windows {
		return strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(prefix))
	}
	return strings.HasPrefix(cleanPath, prefix)
}

// shellQuote wraps a word so the shell reads it as one argument, whatever it
// contains. ssh hands ProxyCommand to a shell rather than running it directly,
// so a path with a space in it — the ordinary case on macOS and Windows —
// would otherwise arrive as two arguments.
//
// Which shell differs, and it is the ssh that reads the config rather than this
// process that decides: /bin/sh everywhere but Windows, where OpenSSH hands the
// line to %COMSPEC%, which knows double quotes and not single ones. A config a
// WSL process writes for Windows is quoted for Windows. A Windows path cannot
// contain a double quote, so there is nothing to escape inside them.
func shellQuote(word string, windows bool) string {
	if windows {
		return `"` + word + `"`
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}
