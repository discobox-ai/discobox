// Command discobox-gzip compresses one file to another with gzip.
//
// It exists because the build cannot rely on the gzip(1) it would otherwise
// call. Windows ships no gzip, the one Git provides is not on PATH in a
// PowerShell or cmd session, and writing its output through a shell redirect
// depends on how the task runner's shell handles redirection on that platform --
// a gzip whose stdout reaches a console refuses to write at all. Doing the work
// in Go removes every one of those variables, and the Go toolchain is already
// required to build anything here.
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: discobox-gzip <source> <destination>")
		os.Exit(2)
	}
	if err := compress(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-gzip: %v\n", err)
		os.Exit(1)
	}
}

func compress(sourcePath, destinationPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	// Written to a temporary file and renamed, so an interrupted run leaves the
	// previous artifact intact rather than a truncated one that looks valid.
	temporary := destinationPath + ".tmp"
	destination, err := os.Create(temporary)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)

	writer, err := gzip.NewWriterLevel(destination, gzip.BestCompression)
	if err != nil {
		destination.Close()
		return err
	}
	if _, err := io.Copy(writer, source); err != nil {
		writer.Close()
		destination.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, destinationPath)
}
