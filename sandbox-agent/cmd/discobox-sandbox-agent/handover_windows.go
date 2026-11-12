package main

import (
	"errors"
	"io/fs"
)

// handOver has no Windows form: nothing there drops to an account.
func handOver(string, int, int, fs.FileMode) error {
	return errors.New("an agent hands nothing over on windows")
}

// handOverWithFiles has no Windows form, for the same reason.
func handOverWithFiles(string, int, int) error {
	return errors.New("an agent hands nothing over on windows")
}

// bootstrapIsRoots has no Windows form, for the same reason.
func bootstrapIsRoots(string) error {
	return errors.New("an agent drops to no account on windows")
}
