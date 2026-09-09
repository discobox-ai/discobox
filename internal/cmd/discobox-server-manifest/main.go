// Command discobox-server-manifest writes the server manifest a release CLI is
// linked with (ADR 0099).
//
// It is run between building a platform's server binary and linking that
// platform's CLI, on the runner that builds both: it hashes the assets that
// were just built, states where the release will publish them, and prints the
// base64 the linker carries into the CLI as
// serverstage.DefaultManifest.
//
//	discobox-server-manifest \
//	  -version v1.2.3 -os linux -arch amd64 \
//	  -base-url https://github.com/discobox-ai/discobox/releases/download/v1.2.3 \
//	  -command discobox-server \
//	  -asset discobox-server=build/release/bin/discobox-server-linux-amd64
//
// The staged name and the published name are deliberately separate: a release
// asset has to say which platform it is for, and a staged file has to be called
// what the thing that runs it expects. So an asset is named on the left of the
// = as the CLI will know it, and the file on the right is both what is hashed
// and — by its own name — what the URL points at.
//
// Encoding here rather than in a shell script because it goes through
// serverstage.EncodeManifest, which is the same validation the CLI applies when
// it decodes one. A manifest that cannot be staged fails the build that made it
// rather than the first user who runs it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/serverstage"
)

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-server-manifest:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		version    = flag.String("version", "", "release version the assets belong to")
		targetOS   = flag.String("os", "", "GOOS the assets are for")
		targetArch = flag.String("arch", "", "GOARCH the assets are for")
		baseURL    = flag.String("base-url", "", "URL the assets are published under, without a trailing slash")
		command    = flag.String("command", "", "staged name of the asset to run")
		assets     stringList
		executable stringList
	)
	flag.Var(&assets, "asset", "staged name=path of a built asset (repeatable)")
	flag.Var(&executable, "executable", "staged name of an asset to stage with the execute bit (repeatable; the command always is)")
	flag.Parse()

	manifest := serverstage.Manifest{
		Version: *version,
		OS:      *targetOS,
		Arch:    *targetArch,
		Command: *command,
	}
	for _, entry := range assets {
		name, path, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("-asset %q is not name=path", entry)
		}
		digest, err := sha256File(path)
		if err != nil {
			return err
		}
		manifest.Assets = append(manifest.Assets, serverstage.Asset{
			Name:       name,
			URL:        strings.TrimSuffix(*baseURL, "/") + "/" + filepath.Base(path),
			SHA256:     digest,
			Executable: name == *command || slices.Contains(executable, name),
		})
	}
	encoded, err := serverstage.EncodeManifest(manifest)
	if err != nil {
		return err
	}
	fmt.Println(encoded)
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
