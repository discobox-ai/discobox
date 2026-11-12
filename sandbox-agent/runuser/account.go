package runuser

import (
	"errors"
	"fmt"
	osuser "os/user"
	"strconv"
	"strings"
)

// Account is an OS account a process started as root can become: the one
// place outside Resolve that turns an account's name into its ids, for the
// agent's drop (its --drop-to-account) and nothing else. Resolve still
// answers who an exec runs as; this answers who the agent itself turns into
// before it starts any.
type Account struct {
	Name     string
	UID, GID int
	// Groups are the account's group set, its primary group first, at most
	// maxGroups long (see LookupAccount).
	Groups []int
	Home   string
}

// LookupAccount resolves the account named by its name, as the guest's own
// account database has it. It refuses root: an agent that drops to uid 0 has
// dropped nothing, and a bootstrap naming root would leave every exec root.
//
// The group set is the primary group and then the account's others, cut to
// what the platform lets a process hold (maxGroups: 16 on macOS, where
// setgroups refuses more, as its initgroups cuts them), so an admin account in
// many groups does not fail the drop.
func LookupAccount(name string) (Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Account{}, errors.New("no account is named")
	}
	found, err := lookupUser(name)
	if err != nil {
		return Account{}, fmt.Errorf("account %q: %w", name, err)
	}
	uid, err := strconv.Atoi(found.Uid)
	if err != nil {
		return Account{}, fmt.Errorf("account %q has uid %q: %w", name, found.Uid, err)
	}
	if uid == 0 {
		return Account{}, fmt.Errorf("account %q is root (uid 0), which is not an account to drop to", name)
	}
	gid, err := strconv.Atoi(found.Gid)
	if err != nil {
		return Account{}, fmt.Errorf("account %q has gid %q: %w", name, found.Gid, err)
	}
	ids, err := found.GroupIds()
	if err != nil {
		return Account{}, fmt.Errorf("account %q's groups: %w", name, err)
	}
	return Account{Name: found.Username, UID: uid, GID: gid, Groups: groupSet(gid, ids, maxGroups), Home: found.HomeDir}, nil
}

// groupSet is gid and then each of ids that is not it, in order, at most limit
// long.
func groupSet(gid int, ids []string, limit int) []int {
	groups := []int{gid}
	for _, id := range ids {
		if len(groups) >= limit {
			break
		}
		if n, err := strconv.Atoi(id); err == nil && n != gid {
			groups = append(groups, n)
		}
	}
	return groups
}

// lookupUser is os/user.Lookup, a variable so a test can name an account.
var lookupUser = osuser.Lookup
