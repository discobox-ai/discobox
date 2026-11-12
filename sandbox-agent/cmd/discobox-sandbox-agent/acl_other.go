//go:build !darwin && !windows

package main

// clearACL has nothing to clear: a Linux ACL is capped by the group bits of
// the mode (its mask), so a directory whose mode lets nobody but its owner
// write lets nobody else write whatever its ACL says.
func clearACL(string) error { return nil }
