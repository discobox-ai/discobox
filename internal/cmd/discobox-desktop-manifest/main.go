// Command discobox-desktop-manifest writes the desktop manifest a release CLI
// is linked with (ADR 26-11-07-983 §2), beside the server manifest
// discobox-server-manifest writes.
//
// It is run once a platform's desktop app archive is final and before that
// platform's CLI is linked: it hashes the archive, states where the release
// publishes it, and prints the base64 the linker carries into the CLI as
// desktopstage.DefaultManifest.
//
//	discobox-desktop-manifest \
//	  -version v1.2.3 -os linux -arch amd64 \
//	  -command discobox-desktop \
//	  -base-url https://github.com/discobox-ai/discobox/releases/download/v1.2.3 \
//	  -asset build/release/bin/discobox-desktop-linux-amd64.tar.gz
//
// -base-url is repeatable and tried in the order given, as for the server.
// -command is the executable's path inside the archive. The encoding goes
// through desktopstage.EncodeManifest, the validation the CLI applies when it
// decodes one, so a manifest that cannot be staged fails the build that made
// it.
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
	"strings"

	"github.com/discobox-ai/discobox/pkg/desktopstage"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func main() {
	encoded, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-desktop-manifest:", err)
		os.Exit(1)
	}
	fmt.Println(encoded)
}

func run(args []string) (string, error) {
	flags := flag.NewFlagSet("discobox-desktop-manifest", flag.ContinueOnError)
	var (
		version    = flags.String("version", "", "release version the app belongs to")
		targetOS   = flags.String("os", "", "GOOS the app is for")
		targetArch = flags.String("arch", "", "GOARCH the app is for")
		command    = flags.String("command", "", "the executable's slash-separated path inside the archive")
		asset      = flags.String("asset", "", "the app's .tar.gz archive, as built")
		baseURLs   stringList
	)
	flags.Var(&baseURLs, "base-url", "URL the archive is published under, without a trailing slash (repeatable; tried in the order given)")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *asset == "" {
		return "", errors.New("-asset is required")
	}
	digest, size, err := sha256File(*asset)
	if err != nil {
		return "", err
	}
	name := filepath.Base(*asset)
	urls := make([]string, 0, len(baseURLs))
	for _, base := range baseURLs {
		urls = append(urls, strings.TrimSuffix(base, "/")+"/"+name)
	}
	return desktopstage.EncodeManifest(desktopstage.Manifest{
		Version: *version,
		OS:      *targetOS,
		Arch:    *targetArch,
		Command: *command,
		Asset:   serverstage.Asset{Name: name, URLs: urls, SHA256: digest, Size: size},
	})
}

// sha256File is the file's digest and size, taken from one read.
func sha256File(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}
