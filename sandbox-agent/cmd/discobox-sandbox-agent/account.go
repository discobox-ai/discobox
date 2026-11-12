package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

// dropToAccountFlag asks the agent to become the sandbox's account once its
// bootstrap has named it, where an init starts it as root before anything
// says whose sandbox this is. It is not a platform's: macOS's launch daemon
// sets it, because a macOS sandbox has one account, the agent runs as it,
// and every exec inherits it (ADR 0145 §5); a systemd unit leaves it unset,
// since the Linux agent stays root and starts each exec as the user.
//
// The account usually has sudo, as a Linux sandbox's user does, but the drop
// does not count on it: it holds where the account has no way back to root.
// The account can end its own agent, and its init then starts it as root
// again, so the root half reads nothing the account can write and changes
// nothing a link the account made can point elsewhere:
//
//   - The bootstrap is root's (bootstrapIsRoots): root-owned, written by no
//     one else, in a directory where no one else can replace it. That is
//     what lets root believe the account it names and the directories it
//     names.
//   - The account is given directories, each alone and never what is in
//     them, each opened from / with no link followed (handOver).
//   - Root is never an account to drop to (runuser.LookupAccount).
const dropToAccountFlag = "drop-to-account"

// handedOver are the directories the root half gives the account, alone: the
// agent's runtime directory and the one its kept runtime-config document is
// in, which the launch daemon makes, and what the bootstrap names for the
// account's database and work. The intake's own directory stays root's, with
// the account let in to add files (configDirMode), so the bootstrap in it
// stays root's. What the agent makes under them, it makes as the account.
func handedOver(cfg config.Config) []string {
	dirs := []string{"/run/discobox", filepath.Dir(intake.DefaultLayout().StatePath)}
	if cfg.DatabasePath != "" {
		dirs = append(dirs, filepath.Dir(cfg.DatabasePath))
	}
	if cfg.WorkingRoot != "" {
		dirs = append(dirs, cfg.WorkingRoot)
	}
	return dirs
}

// configDirMode is the intake's directory, where the account's agent adds the
// readiness marker and its proxy and TLS directories beside the bootstrap:
// root's, the account's group let in to add, and sticky, so nothing in it
// that is not the account's own can be removed or renamed by it.
const configDirMode = os.ModeSticky | 0o775

// dropToAccount makes this process the bootstrap's account. An agent already
// running as it has nothing to do; one running as anyone but root cannot
// become it. Root first checks the bootstrap is root's, hands the account its
// directories, and only then takes the account's groups, gid and uid; the
// directories the agent makes from then on are the account's own.
func dropToAccount(cfg config.Config, configPath string) (runuser.Account, bool, error) {
	acct, err := runuser.LookupAccount(cfg.ExecDefaults.Username)
	if err != nil {
		return runuser.Account{}, false, fmt.Errorf("the bootstrap's account: %w", err)
	}
	euid := os.Geteuid()
	if euid == acct.UID {
		return acct, false, ensureOwnDirs(cfg)
	}
	if euid != 0 {
		return runuser.Account{}, false, fmt.Errorf("the agent runs as uid %d, which is neither root nor the bootstrap's account %q (uid %d)", euid, acct.Name, acct.UID)
	}
	if err := bootstrapIsRoots(configPath); err != nil {
		return runuser.Account{}, false, err
	}
	if err := handOver(sandboxconfig.SandboxConfigDir, 0, acct.GID, configDirMode); err != nil {
		return runuser.Account{}, false, err
	}
	for _, dir := range handedOver(cfg) {
		if err := handOver(dir, acct.UID, acct.GID, 0); err != nil {
			return runuser.Account{}, false, err
		}
	}
	// What the pool placed for the agent beside the bootstrap, its TLS key
	// root's alone among it, which the agent reads and the intake rewrites.
	layout := intake.DefaultLayout()
	for _, dir := range []string{layout.AgentTLSDir, layout.ProxyDir} {
		if err := handOverWithFiles(dir, acct.UID, acct.GID); err != nil {
			return runuser.Account{}, false, err
		}
	}
	if err := runuser.Become(acct); err != nil {
		return runuser.Account{}, false, fmt.Errorf("become %q: %w", acct.Name, err)
	}
	for key, value := range map[string]string{"HOME": acct.Home, "USER": acct.Name, "LOGNAME": acct.Name} {
		if value != "" {
			_ = os.Setenv(key, value)
		}
	}
	return acct, true, ensureOwnDirs(cfg)
}

// ensureOwnDirs makes, as the account, what the agent writes under the
// directories it was handed: the terminals' directory the bootstrap names,
// private to the account, as the Linux unit's pre-start makes it. Made here
// rather than by root, because it sits in a directory the account owns.
func ensureOwnDirs(cfg config.Config) error {
	if cfg.RuntimeDir == "" {
		return nil
	}
	if err := os.MkdirAll(cfg.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", cfg.RuntimeDir, err)
	}
	return nil
}
