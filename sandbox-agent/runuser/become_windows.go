package runuser

import "errors"

// maxGroups has no meaning where nothing can Become an account.
const maxGroups = 1

// Become has no Windows form: a Windows sandbox's agent runs as its one
// account from the start, and changing a process's account there means a
// logon token (ADR 0145 §5).
func Become(Account) error {
	return errors.New("a process cannot change its account on windows")
}
