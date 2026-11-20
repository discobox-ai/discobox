// discobox-harness-bundle stages built-in harness bundles from their
// declarations (ADR 26-11-06-596 §1). The Taskfile's bundle builds, the
// release and the development image watcher build what it stages.
//
//	discobox-harness-bundle stage [-o DIR] HARNESS
//	    Stage HARNESS (its id, such as codex, or its directory, such as
//	    codex-cli) into DIR, by default build/harness-bundles/<directory>.
//	    DIR is then the bundle's whole build context; DIR/target names the
//	    Dockerfile stage to build and DIR/image.json is its label.
//	discobox-harness-bundle check
//	    Validate every built-in's declaration and stage each in memory.
//	discobox-harness-bundle list
//	    Print each built-in's id and directory, in seeding order.
//	discobox-harness-bundle resolve HARNESS
//	    Print HARNESS's id and directory, named by either.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/internal/harnessbundle"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-harness-bundle:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: discobox-harness-bundle stage [-o DIR] HARNESS | check | list | resolve HARNESS")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	tree := os.DirFS(filepath.Join(root, "harness"))
	switch args[0] {
	case "stage":
		flags := flag.NewFlagSet("stage", flag.ContinueOnError)
		out := flags.String("o", "", "directory to stage into")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("stage takes one harness")
		}
		dir, err := harnessbundle.Directory(tree, flags.Arg(0))
		if err != nil {
			return err
		}
		bundle, err := harnessbundle.Generate(tree, dir)
		if err != nil {
			return err
		}
		if *out == "" {
			*out = filepath.Join(root, harnessbundle.StageDir, dir)
		}
		return bundle.Write(*out)
	case "check":
		for _, dir := range harness.BuiltinDirectories() {
			if _, err := harnessbundle.Generate(tree, dir); err != nil {
				return err
			}
		}
		return nil
	case "resolve":
		if len(args) != 2 {
			return errors.New("resolve takes one harness")
		}
		dir, err := harnessbundle.Directory(tree, args[1])
		if err != nil {
			return err
		}
		d, err := harness.LoadDeclaration(tree, dir)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", d.ID, d.Directory)
		return nil
	case "list":
		builtins, err := harness.LoadBuiltins(tree)
		if err != nil {
			return err
		}
		for _, d := range builtins {
			fmt.Printf("%s %s\n", d.ID, d.Directory)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// repoRoot is the checkout this runs in: the nearest directory up from the
// working directory with harness/Dockerfile.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "harness", harnessbundle.Dockerfile)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside a discobox checkout")
		}
		dir = parent
	}
}
