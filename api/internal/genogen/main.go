// Command genogen runs ogen for one OpenAPI document and puts its output in
// the target package without touching a file whose content did not change.
//
// ogen's own --clean deletes every generated file in the target and writes
// each again, so a run that changes nothing still shows the repository's
// hooks a deleted, recreated or truncated Go file and sets off every hook
// over the tree. Generating into an empty temporary directory and syncing it
// in with genfile writes only what differs, and removes a generated file the
// document no longer needs, which is what --clean was for.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/discobox-ai/discobox/pkg/genfile"
)

func main() {
	config := flag.String("config", "", "ogen config file")
	target := flag.String("target", "", "package directory the generated files go in")
	pkg := flag.String("package", "", "generated package name")
	flag.Parse()
	if *config == "" || *target == "" || *pkg == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: genogen -config FILE -target DIR -package NAME SPEC")
		os.Exit(2)
	}
	if err := run(*config, *target, *pkg, flag.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, "genogen:", err)
		os.Exit(1)
	}
}

func run(config, target, pkg, spec string) error {
	tmp, err := os.MkdirTemp("", "genogen-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// ogen is a tool of the root module, which is where go generate runs this.
	cmd := exec.CommandContext(context.Background(), "go", "tool", "ogen", "--config", config, "--target", tmp, "--package", pkg, spec)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ogen %s: %w", spec, err)
	}
	return genfile.SyncDir(tmp, target, ogenOwns)
}

// ogenOwns is the set of files ogen's --clean removes from a target
// (cmd/ogen cleanDir): those it generated, and no others.
func ogenOwns(name string) bool {
	return (strings.HasPrefix(name, "oas") || strings.HasPrefix(name, "openapi")) &&
		(strings.HasSuffix(name, "_gen.go") || strings.HasSuffix(name, "_gen_test.go"))
}
