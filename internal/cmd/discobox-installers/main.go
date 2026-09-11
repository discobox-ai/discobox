// Command discobox-installers writes the install scripts a release uploads,
// stamped with that release and the SHA-256 of every binary they may install
// (ADR 0109).
//
//	discobox-installers -version v1.2.3 -out build/release/bin \
//	  build/release/bin/discobox-linux-amd64 \
//	  build/release/bin/discobox-darwin-arm64 \
//	  build/release/bin/discobox-windows-amd64.exe
//
// release:installers runs it over the CLI binaries release:publish is about to
// upload, so every digest an installer checks describes a file that release
// carries. It hashes the files rather than taking digests on the command line
// for the reason discobox-server-manifest does: a digest and a file handed
// over separately can describe two different builds.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/installer"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-installers:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		version = flag.String("version", "", "release the installers install")
		out     = flag.String("out", "", "directory to write install.sh and install.ps1 into")
	)
	flag.Parse()
	if *out == "" {
		return errors.New("-out is required")
	}
	if flag.NArg() == 0 {
		return errors.New("name at least one binary for the installers to install")
	}

	assets := make([]installer.Asset, 0, flag.NArg())
	for _, path := range flag.Args() {
		digest, err := sha256File(path)
		if err != nil {
			return err
		}
		assets = append(assets, installer.Asset{Name: filepath.Base(path), SHA256: digest})
	}
	scripts, err := installer.Stamp(*version, assets)
	if err != nil {
		return err
	}
	for _, name := range []string{installer.ShellName, installer.PowerShellName} {
		mode := os.FileMode(0o644)
		if name == installer.ShellName {
			mode = 0o755
		}
		path := filepath.Join(*out, name)
		if err := os.WriteFile(path, scripts[name], mode); err != nil {
			return err
		}
		fmt.Printf("wrote %s for %s\n", path, *version)
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
