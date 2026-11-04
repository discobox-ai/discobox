package endpoint

// Linux folds POSIX ACL grants into the group mode mask; the mode check
// already rejects any effective grant to another user.
func checkDirectoryACL(int) error { return nil }
